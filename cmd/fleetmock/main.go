package main

import (
	"log"
	"net/http"
	"os"

	"call-station/internal/fleetmock"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = os.Getenv("FLEET_PORT")
	}
	if port == "" {
		port = "8081"
	}

	srv := fleetmock.NewServer()
	mux := srv.Routes()

	log.Printf("Fleet mock listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
