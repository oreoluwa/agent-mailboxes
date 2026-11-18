package store

import (
	"context"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestAuthenticateTokenRejectsDisabledAliasOrMailbox(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Postgres{DB: db}
	tokenHash := "hash-disabled"
	mock.ExpectQuery("FROM tokens t").
		WithArgs(tokenHash).
		WillReturnRows(sqlmock.NewRows([]string{"id", "alias_id", "scopes"}))

	_, err = p.AuthenticateToken(context.Background(), tokenHash)
	if err != ErrUnauthorized {
		t.Fatalf("expected unauthorized, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateTokenAllowsAdminTokenWithoutAlias(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Postgres{DB: db}
	tokenHash := "hash-admin"
	rows := sqlmock.NewRows([]string{"id", "alias_id", "scopes"}).
		AddRow("tok-admin", nil, "{admin}")
	mock.ExpectQuery("FROM tokens t").
		WithArgs(tokenHash).
		WillReturnRows(rows)

	claims, err := p.AuthenticateToken(context.Background(), tokenHash)
	if err != nil {
		t.Fatalf("authenticate admin token: %v", err)
	}
	if claims.TokenID != "tok-admin" {
		t.Fatalf("unexpected token id: %s", claims.TokenID)
	}
	if claims.AliasID != nil {
		t.Fatalf("expected nil alias id for admin token")
	}
	if !claims.IsAdmin() {
		t.Fatalf("expected admin scope")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
