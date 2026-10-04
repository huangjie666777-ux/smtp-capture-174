package main

import (
	"log"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	httpServer := NewHTTPServer(cfg, store, NewListenerLimiter(cfg.MaxConnections))
	smtpServer := NewSMTPServer(cfg, store, NewListenerLimiter(cfg.MaxConnections))

	go func() {
		log.Printf("HTTP listening on %s", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil {
			log.Fatal(err)
		}
	}()
	log.Printf("SMTP listening on %s", cfg.SMTPAddr)
	if err := smtpServer.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
