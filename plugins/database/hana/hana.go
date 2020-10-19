package hana

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/sdk/database/helper/connutil"
	"github.com/hashicorp/vault/sdk/database/helper/credsutil"
	"github.com/hashicorp/vault/sdk/database/helper/dbutil"
	"github.com/hashicorp/vault/sdk/database/newdbplugin"
	"github.com/hashicorp/vault/sdk/helper/dbtxn"
	"github.com/hashicorp/vault/sdk/helper/strutil"

	_ "github.com/SAP/go-hdb/driver"
)

const (
	hanaTypeName        = "hdb"
	maxIdentifierLength = 127
)

// HANA is an implementation of Database interface
type HANA struct {
	*connutil.SQLConnectionProducer
}

var _ newdbplugin.Database = &HANA{}

// New implements builtinplugins.BuiltinFactory
func New() (interface{}, error) {
	db := new()
	// Wrap the plugin with middleware to sanitize errors
	dbType := newdbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues)

	return dbType, nil
}

func new() *HANA {
	connProducer := &connutil.SQLConnectionProducer{}
	connProducer.Type = hanaTypeName

	return &HANA{
		SQLConnectionProducer: connProducer,
	}
}

func (h *HANA) secretValues() map[string]string {
	return map[string]string{
		h.Password: "[password]",
	}
}

func (h *HANA) Initialize(ctx context.Context, req newdbplugin.InitializeRequest) (newdbplugin.InitializeResponse, error) {
	conf, err := h.Init(ctx, req.Config, req.VerifyConnection)
	if err != nil {
		return newdbplugin.InitializeResponse{}, fmt.Errorf("error initializing db: %s", err)
	}

	return newdbplugin.InitializeResponse{
		Config: conf,
	}, nil
}

// Run instantiates a HANA object, and runs the RPC server for the plugin
func Run(apiTLSConfig *api.TLSConfig) error {
	dbType, err := New()
	if err != nil {
		return err
	}

	newdbplugin.Serve(dbType.(newdbplugin.Database), api.VaultPluginTLSProvider(apiTLSConfig))

	return nil
}

// Type returns the TypeName for this backend
func (h *HANA) Type() (string, error) {
	return hanaTypeName, nil
}

func (h *HANA) getConnection(ctx context.Context) (*sql.DB, error) {
	db, err := h.Connection(ctx)
	if err != nil {
		return nil, err
	}

	return db.(*sql.DB), nil
}

// CreateUser generates the username/password on the underlying HANA secret backend
// as instructed by the CreationStatement provided.
func (h *HANA) NewUser(ctx context.Context, req newdbplugin.NewUserRequest) (response newdbplugin.NewUserResponse, err error) {
	// Grab the lock
	h.Lock()
	defer h.Unlock()

	// Get the connection
	db, err := h.getConnection(ctx)
	if err != nil {
		return newdbplugin.NewUserResponse{}, err
	}

	if len(req.Statements.Commands) == 0 {
		return newdbplugin.NewUserResponse{}, dbutil.ErrEmptyCreationStatement
	}

	dispName := credsutil.DisplayName(req.UsernameConfig.DisplayName, 32)
	roleName := credsutil.RoleName(req.UsernameConfig.RoleName, 20)
	maxLen := credsutil.MaxLength(maxIdentifierLength)
	separator := credsutil.Separator("_")
	caps := credsutil.ToUpper()

	// Generate username
	username, err := credsutil.GenerateUsername(dispName, roleName, maxLen, separator, caps)
	if err != nil {
		return newdbplugin.NewUserResponse{}, err
	}

	// HANA does not allow hyphens in usernames, and highly prefers capital letters
	username = strings.Replace(username, "-", "_", -1)
	username = strings.ToUpper(username)

	// Most HANA configurations have password constraints
	// Prefix with A1a to satisfy these constraints. User will be forced to change upon login
	password := req.Password
	password = strings.Replace(password, "-", "_", -1)

	// If expiration is in the role SQL, HANA will deactivate the user when time is up,
	// regardless of whether vault is alive to revoke lease
	expirationStr := req.Expiration.UTC().Format("2006-01-02 15:04:05")

	// Start a transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return newdbplugin.NewUserResponse{}, err
	}
	defer tx.Rollback()

	// Execute each query
	for _, stmt := range req.Statements.Commands {
		for _, query := range strutil.ParseArbitraryStringSlice(stmt, ";") {
			query = strings.TrimSpace(query)
			if len(query) == 0 {
				continue
			}

			m := map[string]string{
				"name":       username,
				"password":   password,
				"expiration": expirationStr,
			}

			if err := dbtxn.ExecuteTxQuery(ctx, tx, m, query); err != nil {
				return newdbplugin.NewUserResponse{}, err
			}
		}
	}

	// Commit the transaction
	if err := tx.Commit(); err != nil {
		return newdbplugin.NewUserResponse{}, err
	}

	resp := newdbplugin.NewUserResponse{
		Username: username,
	}

	return resp, nil
}

// Renewing hana user just means altering user's valid until property
func (h *HANA) UpdateUser(ctx context.Context, req newdbplugin.UpdateUserRequest) (newdbplugin.UpdateUserResponse, error) {
	h.Lock()
	defer h.Unlock()

	// No change requested
	if req.Password == nil && req.Expiration == nil {
		return newdbplugin.UpdateUserResponse{}, nil
	}

	// Get connection
	db, err := h.getConnection(ctx)
	if err != nil {
		return newdbplugin.UpdateUserResponse{}, err
	}

	// Start a transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return newdbplugin.UpdateUserResponse{}, err
	}
	defer tx.Rollback()

	if req.Password != nil {
		err = h.updateUserPassword(ctx, tx, req.Username, req.Password)
		if err != nil {
			return newdbplugin.UpdateUserResponse{}, err
		}
	}

	if req.Expiration != nil {
		err = h.updateUserExpiration(ctx, tx, req.Username, req.Expiration)
		if err != nil {
			return newdbplugin.UpdateUserResponse{}, err
		}
	}

	// Commit the transaction
	if err := tx.Commit(); err != nil {
		return newdbplugin.UpdateUserResponse{}, err
	}

	return newdbplugin.UpdateUserResponse{}, nil
}

func (h *HANA) updateUserPassword(ctx context.Context, tx *sql.Tx, username string, req *newdbplugin.ChangePassword) error {
	password := req.NewPassword

	if username == "" || password == "" {
		return fmt.Errorf("must provide both username and password")
	}

	stmts := req.Statements.Commands
	if len(stmts) == 0 {
		stmts = []string{"ALTER USER {{username}} PASSWORD \"{{password}}\""}
	}

	for _, stmt := range stmts {
		for _, query := range strutil.ParseArbitraryStringSlice(stmt, ";") {
			query = strings.TrimSpace(query)
			if len(query) == 0 {
				continue
			}

			m := map[string]string{
				"name":     username,
				"username": username,
				"password": password,
			}

			if err := dbtxn.ExecuteTxQuery(ctx, tx, m, query); err != nil {
				return fmt.Errorf("failed to execute query: %w", err)
			}
		}
	}

	return nil
}

func (h *HANA) updateUserExpiration(ctx context.Context, tx *sql.Tx, username string, req *newdbplugin.ChangeExpiration) error {
	// If expiration is in the role SQL, HANA will deactivate the user when time is up,
	// regardless of whether vault is alive to revoke lease
	expirationStr := req.NewExpiration.String()

	if username == "" || expirationStr == "" {
		return fmt.Errorf("must provide both username and expiration")
	}

	stmts := req.Statements.Commands
	if len(stmts) == 0 {
		stmts = []string{"ALTER USER {{username}} VALID UNTIL '{{expiration}}'"}
	}

	for _, stmt := range stmts {
		for _, query := range strutil.ParseArbitraryStringSlice(stmt, ";") {
			query = strings.TrimSpace(query)
			if len(query) == 0 {
				continue
			}

			m := map[string]string{
				"name":       username,
				"username":   username,
				"expiration": expirationStr,
			}

			if err := dbtxn.ExecuteTxQuery(ctx, tx, m, query); err != nil {
				return fmt.Errorf("failed to execute query: %w", err)
			}
		}
	}

	return nil
}

// Revoking hana user will deactivate user and try to perform a soft drop
func (h *HANA) DeleteUser(ctx context.Context, req newdbplugin.DeleteUserRequest) (newdbplugin.DeleteUserResponse, error) {
	h.Lock()
	h.Unlock()

	// default revoke will be a soft drop on user
	if len(req.Statements.Commands) == 0 {
		return h.revokeUserDefault(ctx, req)
	}

	// Get connection
	db, err := h.getConnection(ctx)
	if err != nil {
		return newdbplugin.DeleteUserResponse{}, err
	}

	// Start a transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return newdbplugin.DeleteUserResponse{}, err
	}
	defer tx.Rollback()

	// Execute each query
	for _, stmt := range req.Statements.Commands {
		for _, query := range strutil.ParseArbitraryStringSlice(stmt, ";") {
			query = strings.TrimSpace(query)
			if len(query) == 0 {
				continue
			}

			m := map[string]string{
				"name": req.Username,
			}
			if err := dbtxn.ExecuteTxQuery(ctx, tx, m, query); err != nil {
				return newdbplugin.DeleteUserResponse{}, err
			}
		}
	}

	return newdbplugin.DeleteUserResponse{}, tx.Commit()
}

func (h *HANA) revokeUserDefault(ctx context.Context, req newdbplugin.DeleteUserRequest) (newdbplugin.DeleteUserResponse, error) {
	// Get connection
	db, err := h.getConnection(ctx)
	if err != nil {
		return newdbplugin.DeleteUserResponse{}, err
	}

	// Start a transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return newdbplugin.DeleteUserResponse{}, err
	}
	defer tx.Rollback()

	// Disable server login for user
	disableStmt, err := tx.PrepareContext(ctx, fmt.Sprintf("ALTER USER %s DEACTIVATE USER NOW", req.Username))
	if err != nil {
		return newdbplugin.DeleteUserResponse{}, err
	}
	defer disableStmt.Close()
	if _, err := disableStmt.ExecContext(ctx); err != nil {
		return newdbplugin.DeleteUserResponse{}, err
	}

	// Invalidates current sessions and performs soft drop (drop if no dependencies)
	// if hard drop is desired, custom revoke statements should be written for role
	dropStmt, err := tx.PrepareContext(ctx, fmt.Sprintf("DROP USER %s RESTRICT", req.Username))
	if err != nil {
		return newdbplugin.DeleteUserResponse{}, err
	}
	defer dropStmt.Close()
	if _, err := dropStmt.ExecContext(ctx); err != nil {
		return newdbplugin.DeleteUserResponse{}, err
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return newdbplugin.DeleteUserResponse{}, err
	}

	return newdbplugin.DeleteUserResponse{}, nil
}
