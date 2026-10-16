package main

import (
	"context"
	"strings"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

func (a *app) cmdRegister(ctx context.Context, args []string) error {
	fs := a.newFlagSet("register")
	email := fs.String("email", "", "account email")
	password := fs.String("password", "", "account password")
	name := fs.String("display-name", "", "display name (default: the part of the email before @)")
	terms := fs.String("terms-version", "", "terms version to accept (default: the current one from GET /auth/terms)")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	if *email == "" || *password == "" {
		return usagef("register: --email and --password are required")
	}
	fullName := *name
	if fullName == "" {
		fullName, _, _ = strings.Cut(*email, "@")
	}
	cc, err := a.open()
	if err != nil {
		return err
	}
	version := *terms
	if version == "" {
		if version, err = cc.Terms(ctx); err != nil {
			return err
		}
	}
	user, err := cc.Register(ctx, kotokiclient.RegisterRequest{
		Email: *email, Password: *password, FullName: fullName, TermsVersion: version,
	})
	if err != nil {
		return err
	}
	if err := cc.persist(*email); err != nil {
		return err
	}
	if a.g.JSON {
		return a.printJSON(map[string]string{"user_id": user.ID, "email": user.Email, "terms_version": version})
	}
	a.printf("registered %s (user %s), terms %s\n", user.Email, user.ID, version)
	return nil
}

func (a *app) cmdLogin(ctx context.Context, args []string) error {
	fs := a.newFlagSet("login")
	email := fs.String("email", "", "account email")
	password := fs.String("password", "", "account password")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	if *email == "" || *password == "" {
		return usagef("login: --email and --password are required")
	}
	cc, err := a.open()
	if err != nil {
		return err
	}
	if err := cc.Login(ctx, *email, *password); err != nil {
		return err
	}
	if err := cc.persist(*email); err != nil {
		return err
	}
	if a.g.JSON {
		return a.printJSON(map[string]string{"email": *email, "api_url": a.g.APIURL})
	}
	a.printf("logged in as %s at %s\n", *email, a.g.APIURL)
	return nil
}

func (a *app) cmdCatalog(ctx context.Context, args []string) error {
	fs := a.newFlagSet("catalog")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	cc, err := a.open()
	if err != nil {
		return err
	}
	var cat kotokiclient.Catalog
	if err = cc.run(ctx, func() (e error) { cat, e = cc.Catalog(ctx); return e }); err != nil {
		return err
	}
	if a.g.JSON {
		return a.printJSON(map[string]any{"topics": cat.Topics, "scenarios": cat.Scenarios, "characters": cat.Characters})
	}
	a.printf("characters:\n")
	for _, c := range cat.Characters {
		a.printf("  %s  %-14s %s\n", c.ID, c.Code, c.Name)
	}
	a.printf("scenarios:\n")
	for _, s := range cat.Scenarios {
		a.printf("  %s  %-24s %s\n", s.ID, s.Code, s.Name)
	}
	return nil
}

func (a *app) cmdBalance(ctx context.Context, args []string) error {
	fs := a.newFlagSet("balance")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	cc, err := a.open()
	if err != nil {
		return err
	}
	var b kotokiclient.Balance
	if err = cc.run(ctx, func() (e error) { b, e = cc.Balance(ctx); return e }); err != nil {
		return err
	}
	if a.g.JSON {
		return a.printJSON(b)
	}
	a.printf("available %ds, reserved %ds\n", b.AvailableSeconds, b.ReservedSeconds)
	return nil
}
