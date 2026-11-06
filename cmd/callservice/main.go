package main

import (
	"log"
	"net/http"
	"os"

	"call-station/internal/callservice"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://callstation:callstation@localhost:5432/callstation?sslmode=disable"
	}

	fleetURL := os.Getenv("FLEET_URL")
	if fleetURL == "" {
		fleetURL = "http://localhost:8081"
	}

	stationsFile := os.Getenv("STATIONS_FILE")
	if stationsFile == "" {
		stationsFile = "stations.yaml"
	}

	srv, err := callservice.NewServer(dbURL, fleetURL, stationsFile)
	if err != nil {
		log.Fatalf("failed to start call service: %v", err)
	}
	defer srv.Close()

	mux := srv.Routes()

	log.Printf("Call service listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
