// Command agenthub-relay is the self-hosted public relay (PRD D8/§9): hubs
// connect OUTBOUND (no public IP / port forwarding), browsers hit the relay,
// and traffic is piped through without being stored (红线 3: 不落地).
//
//	agenthub-relay -addr :443 -tls-cert cert.pem -tls-key key.pem -token <relay-token>
//
// Browsers use https://<relay>/<hubID>/…; the Hub dials
// wss://<relay>/relay/hub/<hubID>?token=<relay-token> via
// `agenthubd -relay wss://… -relay-hub-id … -relay-token …`.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/agenttarmac/agenttarmac/internal/relay"
)

func main() {
	var (
		addr     = flag.String("addr", ":443", "listen address (TLS)")
		cert     = flag.String("tls-cert", "", "TLS certificate PEM (required)")
		key      = flag.String("tls-key", "", "TLS key PEM (required)")
		token    = flag.String("token", os.Getenv("AGENTHUB_RELAY_TOKEN"), "relay token the hub must present")
		insecure = flag.Bool("insecure", false, "serve plain HTTP (development only — PRD §9 requires TLS in production)")
	)
	flag.Parse()
	logger := log.New(os.Stdout, "agenthub-relay ", log.LstdFlags|log.Lmsgprefix)

	srv := relay.New(*token, logger)
	server := &http.Server{Addr: *addr, Handler: srv.Handler()}

	if *insecure {
		logger.Printf("relay listening (INSECURE HTTP — development only) on %s", *addr)
		if err := server.ListenAndServe(); err != nil {
			logger.Fatalf("relay: %v", err)
		}
		return
	}
	if *cert == "" || *key == "" {
		logger.Fatalf("relay: -tls-cert/-tls-key are required (PRD §9 red line: 全程 TLS); use -insecure only for development")
	}
	logger.Printf("relay listening (TLS) on %s", *addr)
	if err := server.ListenAndServeTLS(*cert, *key); err != nil {
		logger.Fatalf("relay: %v", err)
	}
}
