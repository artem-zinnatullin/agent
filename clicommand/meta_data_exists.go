package clicommand

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/buildkite/agent/v3/api"
	"github.com/buildkite/agent/v3/cliconfig"
	"github.com/buildkite/roko"
	"github.com/urfave/cli"
)

var MetaDataExistsHelpDescription = `Usage:

   buildkite-agent meta-data exists <key> [options...]

Description:

   The command exits with a status of 0 if the key has been set, or it will
   exit with a status of 100 if the key doesn't exist.

Example:

   $ buildkite-agent meta-data exists "foo"`

type MetaDataExistsConfig struct {
	Key string `cli:"arg:0" label:"meta-data key" validate:"required"`
	Job string `cli:"job" validate:"required"`

	// Global flags
	Debug       bool     `cli:"debug"`
	LogLevel    string   `cli:"log-level"`
	NoColor     bool     `cli:"no-color"`
	Experiments []string `cli:"experiment" normalize:"list"`
	Profile     string   `cli:"profile"`

	// API config
	DebugHTTP        bool   `cli:"debug-http"`
	AgentAccessToken string `cli:"agent-access-token" validate:"required"`
	Endpoint         string `cli:"endpoint" validate:"required"`
	NoHTTP2          bool   `cli:"no-http2"`
}

var MetaDataExistsCommand = cli.Command{
	Name:        "exists",
	Usage:       "Check to see if the meta data key exists for a build",
	Description: MetaDataExistsHelpDescription,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:   "job",
			Value:  "",
			Usage:  "Which job's build should the meta-data be checked for",
			EnvVar: "BUILDKITE_JOB_ID",
		},

		// API Flags
		AgentAccessTokenFlag,
		EndpointFlag,
		NoHTTP2Flag,
		DebugHTTPFlag,

		// Global flags
		NoColorFlag,
		DebugFlag,
		LogLevelFlag,
		ExperimentsFlag,
		ProfileFlag,
	},
	Action: func(c *cli.Context) {
		ctx := context.Background()

		// The configuration will be loaded into this struct
		cfg := MetaDataExistsConfig{}

		loader := cliconfig.Loader{CLI: c, Config: &cfg}
		warnings, err := loader.Load()
		if err != nil {
			fmt.Printf("%s", err)
			os.Exit(1)
		}

		l := CreateLogger(&cfg)

		// Now that we have a logger, log out the warnings that loading config generated
		for _, warning := range warnings {
			l.Warn("%s", warning)
		}

		// Setup any global configuration options
		done := HandleGlobalFlags(l, cfg)
		defer done()

		// Create the API client
		client := api.NewClient(l, loadAPIClientConfig(cfg, "AgentAccessToken"))

		// Find the meta data value
		var exists *api.MetaDataExists
		var resp *api.Response

		err = roko.NewRetrier(
			roko.WithMaxAttempts(10),
			roko.WithStrategy(roko.Constant(5*time.Second)),
		).DoWithContext(ctx, func(r *roko.Retrier) error {
			exists, resp, err = client.ExistsMetaData(ctx, cfg.Job, cfg.Key)
			if resp != nil && (resp.StatusCode == 401 || resp.StatusCode == 404) {
				r.Break()
			}
			if err != nil {
				l.Warn("%s (%s)", err, r)
				return err
			}
			return nil
		})

		if err != nil {
			l.Fatal("Failed to see if meta-data exists: %s", err)
		}

		// If the meta data didn't exist, exit with an error.
		if !exists.Exists {
			os.Exit(100)
		}
	},
}
