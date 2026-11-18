package store

import (
	"context"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

func TestMustAliasVisibleEnforced(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Postgres{DB: db}
	aliasID := "a1"
	claims := model.TokenClaims{AliasID: &aliasID, Scopes: map[model.Scope]bool{model.ScopeRead: true}}
	mock.ExpectQuery("SELECT EXISTS").WithArgs("m1", "a1").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	err = p.mustAliasVisible(context.Background(), claims, "m1")
	if err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
