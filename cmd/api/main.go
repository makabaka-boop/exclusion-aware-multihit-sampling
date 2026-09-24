// Command api serves the material-lab sampling planner over HTTP.
package main

import (
	"log"
	"net/http"
	"os"

	"materiallab/internal/api"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: api.Handler(),
	}

	log.Printf("material-lab api listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
