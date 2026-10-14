package kong_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/alecthomas/assert/v2"
	"github.com/alecthomas/kong"
)

func mustProfiles(t *testing.T, doc string) *kong.ProfileSet {
	t.Helper()
	set, err := kong.ParseProfiles(strings.NewReader(doc))
	assert.NoError(t, err)
	return set
}

type tlsCmd struct {
	Cert string
}

type serveCmd struct {
	Port     string
	LogLevel string `enum:"debug,info,warn" default:"info"`
	TLS      tlsCmd `cmd:""`
}

func (serveCmd) Run() error { return nil }

type deployCmd struct {
	Slot string
}

func (deployCmd) Run() error { return nil }

type profileCLI struct {
	Verbose bool
	Addr    string    `default:"default-addr"`
	Level   string    `enum:"debug,info,warn" default:"info"`
	Serve   serveCmd  `cmd:""`
	Deploy  deployCmd `cmd:""`
}

func (profileCLI) Run() error { return nil }

type scopeTwoCmd struct {
	TwoFlag string
}

type scopeOneCmd struct {
	OneFlag string
	Two     scopeTwoCmd `cmd:""`
}

func (scopeOneCmd) Run() error { return nil }

func newProfileCLI(t *testing.T, doc string, options ...kong.Option) (*kong.Kong, *profileCLI) {
	t.Helper()
	var cli profileCLI
	all := append([]kong.Option{kong.Profiles(mustProfiles(t, doc))}, options...)
	p := mustNew(t, &cli, all...)
	return p, &cli
}

func newExpectingProfileError(t *testing.T, cli any, options ...kong.Option) error {
	t.Helper()
	options = append([]kong.Option{
		kong.Name("test"),
		kong.Exit(func(int) { t.Helper(); t.Fatalf("unexpected exit()") }),
	}, options...)
	_, err := kong.New(cli, options...)
	assert.Error(t, err)
	return err
}

func assertProfileError(t *testing.T, err error, kind kong.ProfileErrorKind) *kong.ProfileError {
	t.Helper()
	var perr *kong.ProfileError
	assert.True(t, errors.As(err, &perr), "expected *kong.ProfileError, got: %v", err)
	if perr != nil {
		assert.Equal(t, kind, perr.Kind)
	}
	return perr
}

func TestProfilesInheritanceAndScopes(t *testing.T) {
	doc := `{
		"profiles": {
			"base": {"values": {"addr": "base:8000", "level": "warn"}},
			"dev": {
				"parent": "base",
				"values": {"addr": "dev:8080"},
				"commands": {
					"serve": {"port": "serve:9090", "log-level": "debug"},
					"serve tls": {"cert": "/dev/cert.pem"}
				}
			}
		}
	}`

	p, cli := newProfileCLI(t, doc, kong.Profile("dev"))

	_, err := p.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "dev:8080", cli.Addr)
	assert.Equal(t, "warn", cli.Level)

	_, err = p.Parse([]string{"serve"})
	assert.NoError(t, err)
	assert.Equal(t, "serve:9090", cli.Serve.Port)
	assert.Equal(t, "debug", cli.Serve.LogLevel)
	assert.Equal(t, "dev:8080", cli.Addr)

	_, err = p.Parse([]string{"serve", "tls"})
	assert.NoError(t, err)
	assert.Equal(t, "/dev/cert.pem", cli.Serve.TLS.Cert)
	assert.Equal(t, "serve:9090", cli.Serve.Port)
}

func TestProfilesGrandparentInheritance(t *testing.T) {
	doc := `{
		"profiles": {
			"grandparent": {"values": {"addr": "grand"}},
			"parent": {"parent": "grandparent", "values": {"level": "info"}},
			"child": {"parent": "parent", "commands": {"serve": {"port": "child-serve"}}}
		}
	}`
	p, cli := newProfileCLI(t, doc, kong.Profile("child"))

	_, err := p.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "grand", cli.Addr)

	_, err = p.Parse([]string{"serve"})
	assert.NoError(t, err)
	assert.Equal(t, "child-serve", cli.Serve.Port)
	assert.Equal(t, "info", cli.Level)
}

func TestProfilesPrecedence(t *testing.T) {
	doc := `{"profiles": {"p": {"values": {"addr": "profile"}}}}`
	set := mustProfiles(t, doc)

	t.Run("cli wins over profile", func(t *testing.T) {
		var cli profileCLI
		p := mustNew(t, &cli, kong.Profiles(set), kong.Profile("p"))
		_, err := p.Parse([]string{"--addr=cli"})
		assert.NoError(t, err)
		assert.Equal(t, "cli", cli.Addr)
	})

	t.Run("context resolver wins over profile", func(t *testing.T) {
		var cli profileCLI
		p := mustNew(t, &cli,
			kong.Profiles(set),
			kong.Profile("p"),
			kong.WithBeforeResolve(func(ctx *kong.Context) error {
				ctx.AddResolver(kong.ResolverFunc(func(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
					if flag.Name == "addr" {
						return "context", nil
					}
					return nil, nil
				}))
				return nil
			}),
		)
		_, err := p.Parse(nil)
		assert.NoError(t, err)
		assert.Equal(t, "context", cli.Addr)
	})

	t.Run("global resolver wins over profile", func(t *testing.T) {
		var cli profileCLI
		global := kong.ResolverFunc(func(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
			if flag.Name == "addr" {
				return "global", nil
			}
			return nil, nil
		})
		p := mustNew(t, &cli, kong.Profiles(set), kong.Profile("p"), kong.Resolvers(global))
		_, err := p.Parse(nil)
		assert.NoError(t, err)
		assert.Equal(t, "global", cli.Addr)
	})

	t.Run("profile wins over config file", func(t *testing.T) {
		var cli profileCLI
		config := makeConfig(t, map[string]string{"addr": "file"})
		p := mustNew(t, &cli,
			kong.Configuration(kong.JSON, config),
			kong.Profiles(set),
			kong.Profile("p"),
		)
		_, err := p.Parse(nil)
		assert.NoError(t, err)
		assert.Equal(t, "profile", cli.Addr)
	})

	t.Run("profile wins over envar and default", func(t *testing.T) {
		var cli profileCLI
		t.Setenv("ADDR", "envar")
		p := mustNew(t, &cli, kong.Profiles(set), kong.Profile("p"), kong.DefaultEnvars(""))
		_, err := p.Parse(nil)
		assert.NoError(t, err)
		assert.Equal(t, "profile", cli.Addr)
	})
}

func TestProfilesNoSelectionKeepsLegacyOrder(t *testing.T) {
	// A configured but unselected profile must not change resolver ordering.
	var cli profileCLI
	doc := `{"profiles": {"p": {"values": {"addr": "profile"}}}}`
	config := makeConfig(t, map[string]string{"addr": "file"})
	p := mustNew(t, &cli,
		kong.Configuration(kong.JSON, config),
		kong.Profiles(mustProfiles(t, doc)),
	)
	_, err := p.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "file", cli.Addr)
	assert.Equal(t, "", p.ActiveProfile())
}

func TestProfilesDefaultProfile(t *testing.T) {
	var cli profileCLI
	doc := `{
		"default_profile": "dev",
		"profiles": {
			"dev": {"values": {"addr": "dev"}},
			"prod": {"values": {"addr": "prod"}}
		}
	}`
	p := mustNew(t, &cli, kong.Profiles(mustProfiles(t, doc)))
	ctx, err := p.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "dev", cli.Addr)
	explanation := ctx.ProfileExplanation()
	assert.NotZero(t, explanation)
	assert.Equal(t, "dev", explanation.Selected)
	assert.True(t, explanation.DefaultSelected)

	assert.NoError(t, p.SelectProfile("prod"))
	_, err = p.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "prod", cli.Addr)
}

func TestProfilesSelectUnknown(t *testing.T) {
	p, _ := newProfileCLI(t, `{"profiles": {"dev": {}}}`)
	err := p.SelectProfile("staging")
	perr := assertProfileError(t, err, kong.ProfileUnknown)
	assert.Equal(t, "staging", perr.Profile)
	assert.Contains(t, err.Error(), "dev")

	// Selection through the Profile() option is validated at construction.
	var cli profileCLI
	err = newExpectingProfileError(t, &cli,
		kong.Profiles(mustProfiles(t, `{"profiles": {"dev": {}}}`)),
		kong.Profile("staging"),
	)
	_ = assertProfileError(t, err, kong.ProfileUnknown)
}

func TestProfilesUnknownParent(t *testing.T) {
	var cli profileCLI
	doc := `{"profiles": {"dev": {"parent": "missing", "values": {"addr": "x"}}}}`
	err := newExpectingProfileError(t, &cli, kong.Profiles(mustProfiles(t, doc)))
	perr := assertProfileError(t, err, kong.ProfileUnknown)
	assert.Contains(t, perr.Profile+perr.Detail, "missing")
	assert.Contains(t, err.Error(), `"dev"`)
}

func TestProfilesCycle(t *testing.T) {
	var cli profileCLI
	doc := `{"profiles": {
		"a": {"parent": "c"},
		"b": {"parent": "a"},
		"c": {"parent": "b"}
	}}`
	err := newExpectingProfileError(t, &cli, kong.Profiles(mustProfiles(t, doc)), kong.Profile("a"))
	_ = assertProfileError(t, err, kong.ProfileCycle)
	assert.Contains(t, err.Error(), "cycle")
}

func TestProfilesDuplicateParent(t *testing.T) {
	t.Run("listed twice", func(t *testing.T) {
		doc := `{"profiles": {
			"base": {},
			"dev": {"parents": ["base", "base"]}
		}}`
		_, err := kong.ParseProfiles(strings.NewReader(doc))
		_ = assertProfileError(t, err, kong.ProfileDuplicateParent)
	})

	t.Run("diamond inheritance", func(t *testing.T) {
		var cli profileCLI
		doc := `{"profiles": {
			"root": {},
			"left": {"parent": "root"},
			"right": {"parent": "root"},
			"leaf": {"parents": ["left", "right"]}
		}}`
		err := newExpectingProfileError(t, &cli, kong.Profiles(mustProfiles(t, doc)), kong.Profile("leaf"))
		_ = assertProfileError(t, err, kong.ProfileDuplicateParent)
	})

	t.Run("parent and parents", func(t *testing.T) {
		doc := `{"profiles": {
			"base": {},
			"dev": {"parent": "base", "parents": ["base"]}
		}}`
		_, err := kong.ParseProfiles(strings.NewReader(doc))
		_ = assertProfileError(t, err, kong.ProfileDuplicateParent)
	})
}

func TestProfilesUnknownField(t *testing.T) {
	var cli profileCLI
	doc := `{"profiles": {"dev": {"values": {"adrr": ":8080"}}}}`
	err := newExpectingProfileError(t, &cli, kong.Profiles(mustProfiles(t, doc)), kong.Profile("dev"))
	perr := assertProfileError(t, err, kong.ProfileUnknownField)
	assert.Equal(t, "adrr", perr.Field)
	assert.Equal(t, "dev", perr.Profile)
	assert.Contains(t, err.Error(), "adrr")
}

func TestProfilesFieldAtOtherCommand(t *testing.T) {
	var cli profileCLI
	doc := `{"profiles": {"dev": {"commands": {"deploy": {"cert": "/x.pem"}}}}}`
	err := newExpectingProfileError(t, &cli, kong.Profiles(mustProfiles(t, doc)), kong.Profile("dev"))
	perr := assertProfileError(t, err, kong.ProfileFieldAtOtherCommand)
	assert.Equal(t, "cert", perr.Field)
	assert.Equal(t, "deploy", perr.Command)
	assert.Contains(t, err.Error(), "serve tls")
}

func TestProfilesUnknownScope(t *testing.T) {
	var cli profileCLI
	doc := `{"profiles": {"dev": {"commands": {"serv": {"port": "x"}}}}}`
	err := newExpectingProfileError(t, &cli, kong.Profiles(mustProfiles(t, doc)))
	perr := assertProfileError(t, err, kong.ProfileUnknownScope)
	assert.Equal(t, "serv", perr.Command)
}

func TestProfilesTypeMismatch(t *testing.T) {
	var cli profileCLI
	doc := `{"profiles": {"dev": {"values": {"verbose": "not-a-bool"}}}}`
	err := newExpectingProfileError(t, &cli, kong.Profiles(mustProfiles(t, doc)))
	perr := assertProfileError(t, err, kong.ProfileTypeMismatch)
	assert.Equal(t, "verbose", perr.Field)
}

func TestProfilesInvalidEnum(t *testing.T) {
	var cli profileCLI
	doc := `{"profiles": {"dev": {"commands": {"serve": {"log-level": "trace"}}}}}`
	err := newExpectingProfileError(t, &cli, kong.Profiles(mustProfiles(t, doc)))
	perr := assertProfileError(t, err, kong.ProfileInvalidEnum)
	assert.Equal(t, "log-level", perr.Field)
	assert.Equal(t, "serve", perr.Command)
}

func TestProfilesXorConflict(t *testing.T) {
	t.Run("global conflict", func(t *testing.T) {
		var cli struct {
			A string `xor:"g"`
			B string `xor:"g"`
		}
		doc := `{"profiles": {"p": {"values": {"a": "1", "b": "2"}}}}`
		err := newExpectingProfileError(t, &cli, kong.Profiles(mustProfiles(t, doc)))
		_ = assertProfileError(t, err, kong.ProfileXorConflict)
	})

	t.Run("sibling scopes do not conflict", func(t *testing.T) {
		var cli struct {
			A      string   `xor:"g"`
			B      string   `xor:"g"`
			Serve  struct{} `cmd:""`
			Deploy struct{} `cmd:""`
		}
		// Same xor group on the root node, but each member is only active on a
		// disjoint command path.
		doc := `{"profiles": {"p": {"commands": {
			"serve": {"a": "x"},
			"deploy": {"b": "y"}
		}}}}`
		_ = mustNew(t, &cli, kong.Profiles(mustProfiles(t, doc)), kong.Profile("p"))
	})

	t.Run("global and scoped conflict", func(t *testing.T) {
		var cli struct {
			A     string   `xor:"g"`
			B     string   `xor:"g"`
			Serve struct{} `cmd:""`
		}
		doc := `{"profiles": {"p": {"values": {"a": "1"}, "commands": {"serve": {"b": "2"}}}}}`
		err := newExpectingProfileError(t, &cli, kong.Profiles(mustProfiles(t, doc)))
		perr := assertProfileError(t, err, kong.ProfileXorConflict)
		assert.Equal(t, "serve", perr.Command)
	})
}

func TestProfilesAndConflictAtParse(t *testing.T) {
	var cli struct {
		X string `and:"pair"`
		Y string `and:"pair"`
	}
	doc := `{"profiles": {"p": {"values": {"x": "1"}}}}`
	p := mustNew(t, &cli, kong.Profiles(mustProfiles(t, doc)), kong.Profile("p"))
	_, err := p.Parse(nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "profile")
	assert.Contains(t, err.Error(), `--x`)
	assert.Contains(t, err.Error(), "p")
}

func TestProfilesInvalidDocument(t *testing.T) {
	for _, doc := range []string{
		`{}`,
		`{"default_profile": 1}`,
		`{"profiles": {"p": {"unknown_key": 1}}}`,
		`{"profiles": {"p": {"values": "not-an-object"}}}`,
		`{"profiles": {"p": {"commands": {"serve": "nope"}}}}`,
		`not json`,
	} {
		_, err := kong.ParseProfiles(strings.NewReader(doc))
		assert.Error(t, err, doc)
		var perr *kong.ProfileError
		assert.True(t, errors.As(err, &perr), doc)
		assert.Equal(t, kong.ProfileInvalidDocument, perr.Kind, doc)
	}
}

func TestProfilesAtomicReplace(t *testing.T) {
	doc := `{"profiles": {
		"base": {"values": {"addr": "base"}},
		"dev": {"parent": "base", "values": {"addr": "dev"}}
	}}`
	p, cli := newProfileCLI(t, doc, kong.Profile("dev"))

	_, err := p.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "dev", cli.Addr)

	bad := mustProfiles(t, `{"profiles": {"new": {"values": {"nope": true}}}}`)
	err = p.ReplaceProfiles(bad)
	assert.Error(t, err)

	// The old set must still work after the failed replacement.
	_, err = p.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "dev", cli.Addr)

	good := mustProfiles(t, `{"profiles": {"prod": {"values": {"addr": "prod"}}}}`)
	assert.NoError(t, p.ReplaceProfiles(good))
	err = p.SelectProfile("dev")
	assert.Error(t, err)
	assert.NoError(t, p.SelectProfile("prod"))
	_, err = p.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "prod", cli.Addr)
}

func TestProfilesExplanation(t *testing.T) {
	var cli profileCLI
	doc := `{
		"profiles": {
			"base": {"values": {"addr": "base", "level": "warn"}},
			"dev": {
				"parent": "base",
				"values": {"addr": "dev"},
				"commands": {
					"serve": {"port": "serve", "log-level": "debug"},
					"deploy": {"slot": "deploy-only"}
				}
			}
		}
	}`
	p := mustNew(t, &cli, kong.Profiles(mustProfiles(t, doc)), kong.Profile("dev"))
	ctx, err := p.Parse([]string{"serve"})
	assert.NoError(t, err)

	explanation := ctx.ProfileExplanation()
	assert.NotZero(t, explanation)
	assert.Equal(t, "dev", explanation.Selected)
	assert.Equal(t, []string{"base", "dev"}, explanation.InheritanceChain)
	assert.Equal(t, "serve", explanation.Command)

	var effective, overridden, inactive int
	var port *kong.ProfileValueExplanation
	for i := range explanation.Values {
		value := &explanation.Values[i]
		switch {
		case value.Flag == "port" && value.Profile == "dev" && value.Scope == "serve":
			assert.True(t, value.Effective)
			assert.True(t, value.Active)
			assert.Equal(t, "serve", value.Command)
			effective++
			port = value
		case value.Flag == "addr" && value.Profile == "base" && value.Scope == "" && value.Active:
			overridden++
			assert.False(t, value.Effective)
			assert.Contains(t, value.OverriddenBy, `"dev"`)
		case value.Scope == "deploy":
			assert.False(t, value.Active)
			assert.Contains(t, value.OverriddenBy, "not part")
			inactive++
		}
	}
	assert.NotZero(t, port)
	assert.Equal(t, 1, effective)
	assert.Equal(t, 1, overridden)
	assert.True(t, inactive >= 1)
}

func TestProfilesExplanationShowsCLIOverride(t *testing.T) {
	var cli profileCLI
	doc := `{"profiles": {"p": {"values": {"addr": "profile"}}}}`
	p := mustNew(t, &cli, kong.Profiles(mustProfiles(t, doc)), kong.Profile("p"))
	ctx, err := p.Parse([]string{"--addr=cli"})
	assert.NoError(t, err)

	explanation := ctx.ProfileExplanation()
	assert.NotZero(t, explanation)
	var entry *kong.ProfileValueExplanation
	for i := range explanation.Values {
		if explanation.Values[i].Flag == "addr" {
			entry = &explanation.Values[i]
		}
	}
	assert.NotZero(t, entry)
	assert.False(t, entry.Effective)
	assert.Contains(t, entry.OverriddenBy, "command-line")
}

func TestProfilesDefaultCommandScope(t *testing.T) {
	var cli struct {
		One scopeOneCmd `cmd:"" default:"withargs"`
	}
	doc := `{"profiles": {"p": {"commands": {
		"one": {"one-flag": "one"},
		"one two": {"two-flag": "two"}
	}}}}`
	p := mustNew(t, &cli, kong.Profiles(mustProfiles(t, doc)), kong.Profile("p"))

	// With no arguments the default command "one" is selected, so its scope
	// applies while the deeper scope must not leak.
	ctx, err := p.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "one", cli.One.OneFlag)
	assert.Equal(t, "", cli.One.Two.TwoFlag)
	assert.Equal(t, "one", ctx.ProfileExplanation().Command)

	// Explicitly selecting the nested command switches scopes.
	_, err = p.Parse([]string{"one", "two"})
	assert.NoError(t, err)
	assert.Equal(t, "one", cli.One.OneFlag)
	assert.Equal(t, "two", cli.One.Two.TwoFlag)
}

func TestProfilesNestedAndSnakeCaseKeys(t *testing.T) {
	var cli struct {
		Nested struct {
			Value string
		} `embed:"" prefix:"nested."`
	}
	doc := `{"profiles": {"p": {"values": {"nested": {"value": "nested"}}}}}`
	p := mustNew(t, &cli, kong.Profiles(mustProfiles(t, doc)), kong.Profile("p"))
	_, err := p.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "nested", cli.Nested.Value)

	var cli2 struct {
		SomeFlag string
	}
	doc2 := `{"profiles": {"p": {"values": {"some_flag": "snake"}}}}`
	p = mustNew(t, &cli2, kong.Profiles(mustProfiles(t, doc2)), kong.Profile("p"))
	_, err = p.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "snake", cli2.SomeFlag)
}

func TestProfilesContextAddResolverCoordination(t *testing.T) {
	// A profile-scoped value must be visible to the command even when a
	// context resolver supplies a different flag, and ordering stays
	// context > global > profile.
	var cli struct {
		A string
		B string
	}
	doc := `{"profiles": {"p": {"values": {"a": "profile-a", "b": "profile-b"}}}}`
	global := kong.ResolverFunc(func(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
		if flag.Name == "b" {
			return "global-b", nil
		}
		return nil, nil
	})
	p := mustNew(t, &cli,
		kong.Profiles(mustProfiles(t, doc)),
		kong.Profile("p"),
		kong.Resolvers(global),
		kong.WithBeforeResolve(func(ctx *kong.Context) error {
			ctx.AddResolver(kong.ResolverFunc(func(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
				if flag.Name == "a" {
					return "context-a", nil
				}
				return nil, nil
			}))
			return nil
		}),
	)
	_, err := p.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "context-a", cli.A)
	assert.Equal(t, "global-b", cli.B)
}

func TestProfilesSelectInBeforeResolve(t *testing.T) {
	// Selection may happen in BeforeResolve, the same hook that supports
	// Context.AddResolver.
	var cli profileCLI
	doc := `{"profiles": {"p": {"values": {"addr": "from-hook"}}}}`
	set := mustProfiles(t, doc)
	var parser *kong.Kong
	parser = mustNew(t, &cli,
		kong.Profiles(set),
		kong.WithBeforeResolve(func(*kong.Context) error {
			return parser.SelectProfile("p")
		}),
	)
	ctx, err := parser.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "from-hook", cli.Addr)
	assert.Equal(t, "p", ctx.ProfileExplanation().Selected)
}

func TestProfilesAncestorFlagScopedToCommand(t *testing.T) {
	var cli profileCLI
	// A root-owned flag may be scoped so it only applies on one command path.
	doc := `{"profiles": {"p": {"commands": {
		"serve": {"addr": "serve-addr"}
	}}}}`
	p := mustNew(t, &cli, kong.Profiles(mustProfiles(t, doc)), kong.Profile("p"))

	_, err := p.Parse([]string{"serve"})
	assert.NoError(t, err)
	assert.Equal(t, "serve-addr", cli.Addr)

	_, err = p.Parse([]string{"deploy"})
	assert.NoError(t, err)
	assert.Equal(t, "default-addr", cli.Addr)

	_, err = p.Parse(nil)
	assert.NoError(t, err)
	assert.Equal(t, "default-addr", cli.Addr)
}

func TestProfilesScopeDoesNotLeakAcrossCommands(t *testing.T) {
	var cli profileCLI
	doc := `{"profiles": {"p": {"commands": {
		"serve": {"port": "serve"},
		"deploy": {"slot": "deploy"}
	}}}}`
	p := mustNew(t, &cli, kong.Profiles(mustProfiles(t, doc)), kong.Profile("p"))

	_, err := p.Parse([]string{"serve"})
	assert.NoError(t, err)
	assert.Equal(t, "serve", cli.Serve.Port)
	assert.Equal(t, "", cli.Deploy.Slot)

	_, err = p.Parse([]string{"deploy"})
	assert.NoError(t, err)
	assert.Equal(t, "deploy", cli.Deploy.Slot)
	assert.Equal(t, "", cli.Serve.Port)
}
