package cluster

import (
	"bytes"
	"database/sql"
	"fmt"
	"net"
	"strings"
	"text/template"
	"time"

	"github.com/lib/pq"

	"github.com/zalando/postgres-operator/pkg/spec"
	"github.com/zalando/postgres-operator/pkg/util/constants"
	"github.com/zalando/postgres-operator/pkg/util/retryutil"
)

const (
	getUserSQL = `SELECT a.rolname, COALESCE(a.rolpassword, ''), a.rolsuper, a.rolinherit,
	        a.rolcreaterole, a.rolcreatedb, a.rolcanlogin, s.setconfig,
	        ARRAY(SELECT b.rolname
	              FROM pg_catalog.pg_auth_members m
	              JOIN pg_catalog.pg_authid b ON (m.roleid = b.oid)
	             WHERE m.member = a.oid) as memberof
	 FROM pg_catalog.pg_authid a LEFT JOIN pg_db_role_setting s ON (a.oid = s.setrole AND s.setdatabase = 0::oid)
	 WHERE a.rolname = ANY($1)
	 ORDER BY 1;`

	getDatabasesSQL = `SELECT datname, pg_get_userbyid(datdba) AS owner FROM pg_database;`
	getSchemasSQL   = `SELECT n.nspname AS dbschema FROM pg_catalog.pg_namespace n
			WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' ORDER BY 1`
	getExtensionsSQL = `SELECT e.extname, n.nspname FROM pg_catalog.pg_extension e
	        LEFT JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace ORDER BY 1;`

	createDatabaseSQL       = `CREATE DATABASE "%s" OWNER "%s";`
	createDatabaseSchemaSQL = `SET ROLE TO "%s"; CREATE SCHEMA IF NOT EXISTS "%s" AUTHORIZATION "%s"`
	alterDatabaseOwnerSQL   = `ALTER DATABASE "%s" OWNER TO "%s";`
	createExtensionSQL      = `CREATE EXTENSION IF NOT EXISTS "%s" SCHEMA "%s"`
	alterExtensionSQL       = `ALTER EXTENSION "%s" SET SCHEMA "%s"`

	globalDefaultPrivilegesSQL = `SET ROLE TO "%s";
			ALTER DEFAULT PRIVILEGES GRANT USAGE ON SCHEMAS TO "%s","%s";
			ALTER DEFAULT PRIVILEGES GRANT SELECT ON TABLES TO "%s";
			ALTER DEFAULT PRIVILEGES GRANT SELECT ON SEQUENCES TO "%s";
			ALTER DEFAULT PRIVILEGES GRANT INSERT, UPDATE, DELETE ON TABLES TO "%s";
			ALTER DEFAULT PRIVILEGES GRANT USAGE, UPDATE ON SEQUENCES TO "%s";
			ALTER DEFAULT PRIVILEGES GRANT EXECUTE ON FUNCTIONS TO "%s","%s";
			ALTER DEFAULT PRIVILEGES GRANT USAGE ON TYPES TO "%s","%s";`
	schemaDefaultPrivilegesSQL = `SET ROLE TO "%s";
			GRANT USAGE ON SCHEMA "%s" TO "%s","%s";
			ALTER DEFAULT PRIVILEGES IN SCHEMA "%s" GRANT SELECT ON TABLES TO "%s";
			ALTER DEFAULT PRIVILEGES IN SCHEMA "%s" GRANT SELECT ON SEQUENCES TO "%s";
			ALTER DEFAULT PRIVILEGES IN SCHEMA "%s" GRANT INSERT, UPDATE, DELETE ON TABLES TO "%s";
			ALTER DEFAULT PRIVILEGES IN SCHEMA "%s" GRANT USAGE, UPDATE ON SEQUENCES TO "%s";
			ALTER DEFAULT PRIVILEGES IN SCHEMA "%s" GRANT EXECUTE ON FUNCTIONS TO "%s","%s";
			ALTER DEFAULT PRIVILEGES IN SCHEMA "%s" GRANT USAGE ON TYPES TO "%s","%s";`

	connectionPoolerLookup = `
		CREATE SCHEMA IF NOT EXISTS {{.pooler_schema}};

		CREATE OR REPLACE FUNCTION {{.pooler_schema}}.user_lookup(
			in i_username text, out uname text, out phash text)
		RETURNS record AS $$
		BEGIN
			SELECT usename, passwd FROM pg_catalog.pg_shadow
			WHERE usename = i_username INTO uname, phash;
			RETURN;
		END;
		$$ LANGUAGE plpgsql SECURITY DEFINER;

		REVOKE ALL ON FUNCTION {{.pooler_schema}}.user_lookup(text)
			FROM public, {{.pooler_user}};
		GRANT EXECUTE ON FUNCTION {{.pooler_schema}}.user_lookup(text)
			TO {{.pooler_user}};
		GRANT USAGE ON SCHEMA {{.pooler_schema}} TO {{.pooler_user}};
	`
)

func (c *Cluster) pgConnectionString(dbname string) string {
	password := c.systemUsers[constants.SuperuserKeyName].Password

	if dbname == "" {
		dbname = "postgres"
	}

	return fmt.Sprintf("host='%s' dbname='%s' sslmode=require user='%s' password='%s' connect_timeout='%d'",
		fmt.Sprintf("%s.%s.svc.%s", c.Name, c.Namespace, c.OpConfig.ClusterDomain),
		dbname,
		c.systemUsers[constants.SuperuserKeyName].Name,
		strings.Replace(password, "$", "\\$", -1),
		constants.PostgresConnectTimeout/time.Second)
}

func (c *Cluster) databaseAccessDisabled() bool {
	if !c.OpConfig.EnableDBAccess {
		c.logger.Debugf("database access is disabled")
	}

	return !c.OpConfig.EnableDBAccess
}

func (c *Cluster) initDbConn() error {
	return c.initDbConnWithName("")
}

func (c *Cluster) initDbConnWithName(dbname string) error {
	c.setProcessName("initializing db connection")
	if c.pgDb != nil {
		return nil
	}

	var conn *sql.DB
	connstring := c.pgConnectionString(dbname)

	finalerr := retryutil.Retry(constants.PostgresConnectTimeout, constants.PostgresConnectRetryTimeout,
		func() (bool, error) {
			var err error
			conn, err = sql.Open("postgres", connstring)
			if err == nil {
				err = conn.Ping()
			}

			if err == nil {
				return true, nil
			}

			if _, ok := err.(*net.OpError); ok {
				c.logger.Errorf("could not connect to PostgreSQL database: %v", err)
				return false, nil
			}

			if err2 := conn.Close(); err2 != nil {
				c.logger.Errorf("error when closing PostgreSQL connection after another error: %v", err)
				return false, err2
			}

			return false, err
		})

	if finalerr != nil {
		return fmt.Errorf("could not init db connection: %v", finalerr)
	}
	// Limit ourselves to a single connection and allow no idle connections.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(-1)

	c.pgDb = conn

	return nil
}

func (c *Cluster) connectionIsClosed() bool {
	return c.pgDb == nil
}

func (c *Cluster) closeDbConn() (err error) {
	c.setProcessName("closing db connection")
	if c.pgDb != nil {
		c.logger.Debug("closing database connection")
		if err = c.pgDb.Close(); err != nil {
			c.logger.Errorf("could not close database connection: %v", err)
		}
		c.pgDb = nil

		return nil
	}
	c.logger.Warning("attempted to close an empty db connection object")
	return nil
}

func (c *Cluster) readPgUsersFromDatabase(userNames []string) (users spec.PgUserMap, err error) {
	c.setProcessName("reading users from the db")
	var rows *sql.Rows
	users = make(spec.PgUserMap)
	if rows, err = c.pgDb.Query(getUserSQL, pq.Array(userNames)); err != nil {
		return nil, fmt.Errorf("error when querying users: %v", err)
	}
	defer func() {
		if err2 := rows.Close(); err2 != nil {
			err = fmt.Errorf("error when closing query cursor: %v", err2)
		}
	}()

	for rows.Next() {
		var (
			rolname, rolpassword                                          string
			rolsuper, rolinherit, rolcreaterole, rolcreatedb, rolcanlogin bool
			roloptions, memberof                                          []string
		)
		err := rows.Scan(&rolname, &rolpassword, &rolsuper, &rolinherit,
			&rolcreaterole, &rolcreatedb, &rolcanlogin, pq.Array(&roloptions), pq.Array(&memberof))
		if err != nil {
			return nil, fmt.Errorf("error when processing user rows: %v", err)
		}
		flags := makeUserFlags(rolsuper, rolinherit, rolcreaterole, rolcreatedb, rolcanlogin)
		// XXX: the code assumes the password we get from pg_authid is always MD5
		parameters := make(map[string]string)
		for _, option := range roloptions {
			fields := strings.Split(option, "=")
			if len(fields) != 2 {
				c.logger.Warningf("skipping malformed option: %q", option)
				continue
			}
			parameters[fields[0]] = fields[1]
		}

		users[rolname] = spec.PgUser{Name: rolname, Password: rolpassword, Flags: flags, MemberOf: memberof, Parameters: parameters}
	}

	return users, nil
}

// getDatabases returns the map of current databases with owners
// The caller is responsible for opening and closing the database connection
func (c *Cluster) getDatabases() (dbs map[string]string, err error) {
	var (
		rows *sql.Rows
	)

	if rows, err = c.pgDb.Query(getDatabasesSQL); err != nil {
		return nil, fmt.Errorf("could not query database: %v", err)
	}

	defer func() {
		if err2 := rows.Close(); err2 != nil {
			if err != nil {
				err = fmt.Errorf("error when closing query cursor: %v, previous error: %v", err2, err)
			} else {
				err = fmt.Errorf("error when closing query cursor: %v", err2)
			}
		}
	}()

	dbs = make(map[string]string)

	for rows.Next() {
		var datname, owner string

		if err = rows.Scan(&datname, &owner); err != nil {
			return nil, fmt.Errorf("error when processing row: %v", err)
		}
		dbs[datname] = owner
	}

	return dbs, err
}

// executeCreateDatabase creates new database with the given owner.
// The caller is responsible for opening and closing the database connection.
func (c *Cluster) executeCreateDatabase(databaseName, owner string) error {
	return c.execCreateOrAlterDatabase(databaseName, owner, createDatabaseSQL,
		"creating database", "create database")
}

// executeAlterDatabaseOwner changes the owner of the given database.
// The caller is responsible for opening and closing the database connection.
func (c *Cluster) executeAlterDatabaseOwner(databaseName string, owner string) error {
	return c.execCreateOrAlterDatabase(databaseName, owner, alterDatabaseOwnerSQL,
		"changing owner for database", "alter database owner")
}

func (c *Cluster) execCreateOrAlterDatabase(databaseName, owner, statement, doing, operation string) error {
	if !c.databaseNameOwnerValid(databaseName, owner) {
		return nil
	}
	c.logger.Infof("%s %q owner %q", doing, databaseName, owner)
	if _, err := c.pgDb.Exec(fmt.Sprintf(statement, databaseName, owner)); err != nil {
		return fmt.Errorf("could not execute %s: %v", operation, err)
	}
	return nil
}

func (c *Cluster) databaseNameOwnerValid(databaseName, owner string) bool {
	if _, ok := c.pgUsers[owner]; !ok {
		c.logger.Infof("skipping creation of the %q database, user %q does not exist", databaseName, owner)
		return false
	}

	if !databaseNameRegexp.MatchString(databaseName) {
		c.logger.Infof("database %q has invalid name", databaseName)
		return false
	}
	return true
}

// getSchemas returns the list of current database schemas
// The caller is responsible for opening and closing the database connection
func (c *Cluster) getSchemas() (schemas []string, err error) {
	var (
		rows      *sql.Rows
		dbschemas []string
	)

	if rows, err = c.pgDb.Query(getSchemasSQL); err != nil {
		return nil, fmt.Errorf("could not query database schemas: %v", err)
	}

	defer func() {
		if err2 := rows.Close(); err2 != nil {
			if err != nil {
				err = fmt.Errorf("error when closing query cursor: %v, previous error: %v", err2, err)
			} else {
				err = fmt.Errorf("error when closing query cursor: %v", err2)
			}
		}
	}()

	for rows.Next() {
		var dbschema string

		if err = rows.Scan(&dbschema); err != nil {
			return nil, fmt.Errorf("error when processing row: %v", err)
		}
		dbschemas = append(dbschemas, dbschema)
	}

	return dbschemas, err
}

// executeCreateDatabaseSchema creates new database schema with the given owner.
// The caller is responsible for opening and closing the database connection.
func (c *Cluster) executeCreateDatabaseSchema(databaseName, schemaName, dbOwner string, schemaOwner string) error {
	return c.execCreateDatabaseSchema(databaseName, schemaName, dbOwner, schemaOwner, createDatabaseSchemaSQL,
		"creating database schema", "create database schema")
}

func (c *Cluster) execCreateDatabaseSchema(databaseName, schemaName, dbOwner, schemaOwner, statement, doing, operation string) error {
	if !c.databaseSchemaNameValid(schemaName) {
		return nil
	}
	c.logger.Infof("%s %q owner %q", doing, schemaName, schemaOwner)
	if _, err := c.pgDb.Exec(fmt.Sprintf(statement, dbOwner, schemaName, schemaOwner)); err != nil {
		return fmt.Errorf("could not execute %s: %v", operation, err)
	}

	// set default privileges for schema
	c.execAlterSchemaDefaultPrivileges(schemaName, schemaOwner, databaseName)
	if schemaOwner != dbOwner {
		c.execAlterSchemaDefaultPrivileges(schemaName, dbOwner, databaseName+"_"+schemaName)
		c.execAlterSchemaDefaultPrivileges(schemaName, schemaOwner, databaseName+"_"+schemaName)
	}

	return nil
}

func (c *Cluster) databaseSchemaNameValid(schemaName string) bool {
	if !databaseNameRegexp.MatchString(schemaName) {
		c.logger.Infof("database schema %q has invalid name", schemaName)
		return false
	}
	return true
}

func (c *Cluster) execAlterSchemaDefaultPrivileges(schemaName, owner, rolePrefix string) error {
	if _, err := c.pgDb.Exec(fmt.Sprintf(schemaDefaultPrivilegesSQL, owner,
		schemaName, rolePrefix+constants.ReaderRoleNameSuffix, rolePrefix+constants.WriterRoleNameSuffix, // schema
		schemaName, rolePrefix+constants.ReaderRoleNameSuffix, // tables
		schemaName, rolePrefix+constants.ReaderRoleNameSuffix, // sequences
		schemaName, rolePrefix+constants.WriterRoleNameSuffix, // tables
		schemaName, rolePrefix+constants.WriterRoleNameSuffix, // sequences
		schemaName, rolePrefix+constants.ReaderRoleNameSuffix, rolePrefix+constants.WriterRoleNameSuffix, // types
		schemaName, rolePrefix+constants.ReaderRoleNameSuffix, rolePrefix+constants.WriterRoleNameSuffix)); err != nil { // functions
		return fmt.Errorf("could not alter default privileges for database schema %s: %v", schemaName, err)
	}

	return nil
}

func (c *Cluster) execAlterGlobalDefaultPrivileges(owner, rolePrefix string) error {
	if _, err := c.pgDb.Exec(fmt.Sprintf(globalDefaultPrivilegesSQL, owner,
		rolePrefix+constants.WriterRoleNameSuffix, rolePrefix+constants.ReaderRoleNameSuffix, // schemas
		rolePrefix+constants.ReaderRoleNameSuffix,                                            // tables
		rolePrefix+constants.ReaderRoleNameSuffix,                                            // sequences
		rolePrefix+constants.WriterRoleNameSuffix,                                            // tables
		rolePrefix+constants.WriterRoleNameSuffix,                                            // sequences
		rolePrefix+constants.ReaderRoleNameSuffix, rolePrefix+constants.WriterRoleNameSuffix, // types
		rolePrefix+constants.ReaderRoleNameSuffix, rolePrefix+constants.WriterRoleNameSuffix)); err != nil { // functions
		return fmt.Errorf("could not alter default privileges for database %s: %v", rolePrefix, err)
	}

	return nil
}

func makeUserFlags(rolsuper, rolinherit, rolcreaterole, rolcreatedb, rolcanlogin bool) (result []string) {
	if rolsuper {
		result = append(result, constants.RoleFlagSuperuser)
	}
	if rolinherit {
		result = append(result, constants.RoleFlagInherit)
	}
	if rolcreaterole {
		result = append(result, constants.RoleFlagCreateRole)
	}
	if rolcreatedb {
		result = append(result, constants.RoleFlagCreateDB)
	}
	if rolcanlogin {
		result = append(result, constants.RoleFlagLogin)
	}

	return result
}

// getExtension returns the list of current database extensions
// The caller is responsible for opening and closing the database connection
func (c *Cluster) getExtensions() (dbExtensions map[string]string, err error) {
	var (
		rows *sql.Rows
	)

	if rows, err = c.pgDb.Query(getExtensionsSQL); err != nil {
		return nil, fmt.Errorf("could not query database extensions: %v", err)
	}

	defer func() {
		if err2 := rows.Close(); err2 != nil {
			if err != nil {
				err = fmt.Errorf("error when closing query cursor: %v, previous error: %v", err2, err)
			} else {
				err = fmt.Errorf("error when closing query cursor: %v", err2)
			}
		}
	}()

	dbExtensions = make(map[string]string)

	for rows.Next() {
		var extension, schema string

		if err = rows.Scan(&extension, &schema); err != nil {
			return nil, fmt.Errorf("error when processing row: %v", err)
		}
		dbExtensions[extension] = schema
	}

	return dbExtensions, err
}

// executeCreateExtension creates new extension in the given schema.
// The caller is responsible for opening and closing the database connection.
func (c *Cluster) executeCreateExtension(extName, schemaName string) error {
	return c.execCreateOrAlterExtension(extName, schemaName, createExtensionSQL,
		"creating extension", "create extension")
}

// executeAlterExtension changes the schema of the given extension.
// The caller is responsible for opening and closing the database connection.
func (c *Cluster) executeAlterExtension(extName, schemaName string) error {
	return c.execCreateOrAlterExtension(extName, schemaName, alterExtensionSQL,
		"changing schema for extension", "alter extension schema")
}

func (c *Cluster) execCreateOrAlterExtension(extName, schemaName, statement, doing, operation string) error {

	c.logger.Infof("%s %q schema %q", doing, extName, schemaName)
	if _, err := c.pgDb.Exec(fmt.Sprintf(statement, extName, schemaName)); err != nil {
		return fmt.Errorf("could not execute %s: %v", operation, err)
	}

	return nil
}

// Creates a connection pool credentials lookup function in every database to
// perform remote authentification.
func (c *Cluster) installLookupFunction(poolerSchema, poolerUser string) error {
	var stmtBytes bytes.Buffer
	c.logger.Info("Installing lookup function")

	if err := c.initDbConn(); err != nil {
		return fmt.Errorf("could not init database connection")
	}
	defer func() {
		if c.connectionIsClosed() {
			return
		}

		if err := c.closeDbConn(); err != nil {
			c.logger.Errorf("could not close database connection: %v", err)
		}
	}()

	currentDatabases, err := c.getDatabases()
	if err != nil {
		msg := "could not get databases to install pooler lookup function: %v"
		return fmt.Errorf(msg, err)
	}

	templater := template.Must(template.New("sql").Parse(connectionPoolerLookup))

	for dbname := range currentDatabases {
		if dbname == "template0" || dbname == "template1" {
			continue
		}

		if err := c.initDbConnWithName(dbname); err != nil {
			return fmt.Errorf("could not init database connection to %s", dbname)
		}

		c.logger.Infof("Install pooler lookup function into %s", dbname)

		params := TemplateParams{
			"pooler_schema": poolerSchema,
			"pooler_user":   poolerUser,
		}

		if err := templater.Execute(&stmtBytes, params); err != nil {
			c.logger.Errorf("could not prepare sql statement %+v: %v",
				params, err)
			// process other databases
			continue
		}

		// golang sql will do retries couple of times if pq driver reports
		// connections issues (driver.ErrBadConn), but since our query is
		// idempotent, we can retry in a view of other errors (e.g. due to
		// failover a db is temporary in a read-only mode or so) to make sure
		// it was applied.
		execErr := retryutil.Retry(
			constants.PostgresConnectTimeout,
			constants.PostgresConnectRetryTimeout,
			func() (bool, error) {
				if _, err := c.pgDb.Exec(stmtBytes.String()); err != nil {
					msg := fmt.Errorf("could not execute sql statement %s: %v",
						stmtBytes.String(), err)
					return false, msg
				}

				return true, nil
			})

		if execErr != nil {
			c.logger.Errorf("could not execute after retries %s: %v",
				stmtBytes.String(), err)
			// process other databases
			continue
		}

		c.logger.Infof("pooler lookup function installed into %s", dbname)
		if err := c.closeDbConn(); err != nil {
			c.logger.Errorf("could not close database connection: %v", err)
		}
	}

	c.ConnectionPooler.LookupFunction = true
	return nil
}
