package main

import (
	"log"
	"net/http"
)

type GrepStreamsAPI struct{}

func (GrepStreamsAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("Hello, world"))
}

func main() {
	server := http.Server{
		Handler: GrepStreamsAPI{},
		Addr:    ":8443",
	}
	err := server.ListenAndServeTLS("localhost.crt", "localhost.key")
	if err != nil {
		log.Printf("unclean shutdown! err=%v\n", err)
	}
}
