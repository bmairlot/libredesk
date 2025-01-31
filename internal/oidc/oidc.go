package oidc

import (
	"embed"
	"fmt"
	"strings"

	"github.com/abhinavxd/libredesk/internal/dbutil"
	"github.com/abhinavxd/libredesk/internal/envelope"
	"github.com/abhinavxd/libredesk/internal/oidc/models"
	"github.com/abhinavxd/libredesk/internal/stringutil"
	"github.com/jmoiron/sqlx"
	"github.com/jmoiron/sqlx/types"
	"github.com/zerodha/logf"
)

var (
	//go:embed queries.sql
	efs         embed.FS
	redirectURL = "/api/v1/oidc/%d/finish"
)

// Manager handles oidc-related operations.
type Manager struct {
	q       queries
	lo      *logf.Logger
	setting settingsStore
}

// Opts contains options for initializing the Manager.
type Opts struct {
	DB *sqlx.DB
	Lo *logf.Logger
}

// queries contains prepared SQL queries.
type queries struct {
	GetAllOIDC    *sqlx.Stmt `query:"get-all-oidc"`
	GetAllEnabled *sqlx.Stmt `query:"get-all-enabled"`
	GetOIDC       *sqlx.Stmt `query:"get-oidc"`
	InsertOIDC    *sqlx.Stmt `query:"insert-oidc"`
	UpdateOIDC    *sqlx.Stmt `query:"update-oidc"`
	DeleteOIDC    *sqlx.Stmt `query:"delete-oidc"`
}

type settingsStore interface {
	Get(key string) (types.JSONText, error)
}

// New creates and returns a new instance of the oidc Manager.
func New(opts Opts, setting settingsStore) (*Manager, error) {
	var q queries
	if err := dbutil.ScanSQLFile("queries.sql", &q, opts.DB, efs); err != nil {
		return nil, err
	}
	return &Manager{
		q:       q,
		lo:      opts.Lo,
		setting: setting,
	}, nil
}

// Get returns an oidc by id.
func (o *Manager) Get(id int, includeSecret bool) (models.OIDC, error) {
	var oidc models.OIDC
	if err := o.q.GetOIDC.Get(&oidc, id); err != nil {
		o.lo.Error("error fetching oidc", "error", err)
		return oidc, envelope.NewError(envelope.GeneralError, "Error fetching OIDC", nil)
	}
	oidc.SetProviderLogo()
	if oidc.ClientSecret != "" && !includeSecret {
		oidc.ClientSecret = strings.Repeat(stringutil.PasswordDummy, 10)
	}
	return oidc, nil
}

// GetAll retrieves all oidc.
func (o *Manager) GetAll() ([]models.OIDC, error) {
	var oidc = make([]models.OIDC, 0)
	if err := o.q.GetAllOIDC.Select(&oidc); err != nil {
		o.lo.Error("error fetching oidc", "error", err)
		return oidc, envelope.NewError(envelope.GeneralError, "Error fetching OIDC", nil)
	}

	// Get root URL of the app.
	rootURL, err := o.setting.Get("app.root_url")
	if err != nil {
		o.lo.Error("error fetching root URL", "error", err)
		return oidc, envelope.NewError(envelope.GeneralError, "Error fetching root URL", nil)
	}

	// Strip the quotes from the root URL.
	rootURLStr := strings.Trim(string(rootURL), "\"")

	// Set logo and redirect URL.
	for i := range oidc {
		oidc[i].RedirectURI = fmt.Sprintf(rootURLStr+redirectURL, oidc[i].ID)
		oidc[i].SetProviderLogo()
	}
	return oidc, nil
}

// GetAllEnabled retrieves all enabled oidc.
func (o *Manager) GetAllEnabled() ([]models.OIDC, error) {
	var oidc = make([]models.OIDC, 0)
	if err := o.q.GetAllEnabled.Select(&oidc); err != nil {
		o.lo.Error("error fetching oidc", "error", err)
		return oidc, envelope.NewError(envelope.GeneralError, "Error fetching OIDC", nil)
	}
	for i := range oidc {
		oidc[i].SetProviderLogo()
	}
	return oidc, nil
}

// Create adds a new oidc.
func (o *Manager) Create(oidc models.OIDC) error {
	if _, err := o.q.InsertOIDC.Exec(oidc.Name, oidc.Provider, oidc.ProviderURL, oidc.ClientID, oidc.ClientSecret); err != nil {
		o.lo.Error("error inserting oidc", "error", err)
		return envelope.NewError(envelope.GeneralError, "Error creating OIDC", nil)
	}
	return nil
}

// Create updates a oidc by id.
func (o *Manager) Update(id int, oidc models.OIDC) error {
	current, err := o.Get(id, true)
	if err != nil {
		return err
	}
	if oidc.ClientSecret == "" {
		oidc.ClientSecret = current.ClientSecret
	}
	if _, err := o.q.UpdateOIDC.Exec(id, oidc.Name, oidc.Provider, oidc.ProviderURL, oidc.ClientID, oidc.ClientSecret, oidc.Enabled); err != nil {
		o.lo.Error("error updating oidc", "error", err)
		return envelope.NewError(envelope.GeneralError, "Error updating OIDC", nil)
	}
	return nil
}

// Delete deletes a oidc by its id.
func (o *Manager) Delete(id int) error {
	if _, err := o.q.DeleteOIDC.Exec(id); err != nil {
		o.lo.Error("error deleting oidc", "error", err)
		return envelope.NewError(envelope.GeneralError, "Error fetching OIDC", nil)
	}
	return nil
}
