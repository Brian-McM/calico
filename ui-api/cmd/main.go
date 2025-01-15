package main

import (
	"context"
	"github.com/gorilla/mux"
	"github.com/projectcalico/calico/goldmane/proto"
	"github.com/projectcalico/calico/lib/apimachinery/pkg/server"
	gorillaadpt "github.com/projectcalico/calico/lib/apimachinery/pkg/server/adaptors/gorilla"
	"github.com/projectcalico/calico/ui-api/pkg/handlers/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	conn, err := grpc.NewClient("goldmane.calico-system.svc:7443", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		panic(err)
	}

	apiCli := proto.NewFlowAPIClient(conn)

	flowsAPI := v1.NewFlows(apiCli)

	srv, err := server.NewHTTPServer(
		gorillaadpt.NewRouter(mux.NewRouter()),
		flowsAPI.APIs(),
		server.WithAddr(":3002"),
		//server.WithTLSFiles("/home/brian-mcmahon/go-private/src/github.com/projectcalico/calico/ui-api/test/cert.pem", "/home/brian-mcmahon/go-private/src/github.com/projectcalico/calico/ui-api/test/cert.key")
	)
	if err != nil {
		panic(err)
	}

	if err := srv.ListenAndServe(context.Background()); err != nil {
		panic(err)
	}

	if err := srv.WaitForShutdown(); err != nil {
		panic(err)
	}
}
