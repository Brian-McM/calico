package main

import (
	"context"
	"log"

	"github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/goldmane/pkg/client"
	"github.com/projectcalico/calico/lib/httpapimachinery/pkg/server"
	gorillaadpt "github.com/projectcalico/calico/lib/httpapimachinery/pkg/server/adaptors/gorilla"
	"github.com/projectcalico/calico/prospector/pkg/config"
	"github.com/projectcalico/calico/prospector/pkg/handlers/v1"
)

func main() {
	cfg, err := config.NewConfig()
	if err != nil {
		log.Fatal(err)
	}
	logrus.WithField("cfg", cfg.String()).Info("Configuring prospector...")

	gmCli, err := client.NewFlowsAPIClient(cfg.GoldmaneHost)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to create goldmane client.")
	}

	flowsAPI := v1.NewFlows(gmCli)

	opts := []server.Option{
		server.WithAddr(cfg.HostAddr()),
	}

	// TODO maybe we can push getting tls files to the common http utilities package?
	if cfg.TlsKeyPath != "" && cfg.TlsCertPath != "" {
		opts = append(opts, server.WithTLSFiles(cfg.TlsCertPath, cfg.TlsKeyPath))
	}

	srv, err := server.NewHTTPServer(
		gorillaadpt.NewRouter(),
		flowsAPI.APIs(),
		opts...,
	)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to create server.")
	}

	// TODO need to make this TLS.
	logrus.Infof("Listening on %s.", cfg.HostAddr())
	if err := srv.ListenAndServe(context.Background()); err != nil {
		logrus.WithError(err).Fatal("Failed to start server.")
	}

	if err := srv.WaitForShutdown(); err != nil {
		logrus.WithError(err).Fatal("An unexpected error occurred while waiting for shutdown.")
	}
}
