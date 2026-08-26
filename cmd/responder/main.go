package main

import (
	"os"

	natsauthcallout "github.com/truvity/nats-auth-callout/pkg/nats-auth-callout"
)

var (
	// Version is set via ldflags during build (-X main.Version={{.Version}})
	Version = "dev"
	// GitCommit is set via ldflags during build (-X main.GitCommit={{.Commit}})
	GitCommit = "unknown"
)

func main() {
	os.Exit(natsauthcallout.Run(os.Args, Version, GitCommit))
}
