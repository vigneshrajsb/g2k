package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

type Webhook struct {
	Action string `json:"action"`
}

func main() {
	config := LoadConfig()

	consumer, err := kafka.NewConsumer(&config)
	if err != nil {
		log.Fatalf("Failed to create consumer: %s", err)
	}
	defer consumer.Close()

	err = consumer.SubscribeTopics([]string{"github.events"}, nil)
	if err != nil {
		log.Fatalf("Failed to subscribe to topics: %s", err)
	}

	allowedRepos := buildAllowedReposSet()

	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, syscall.SIGINT, syscall.SIGTERM)

	log.Println("Consumer started. Waiting for messages...")

	run := true

	for run {
		select {
		case sig := <-sigchan:
			log.Printf("Caught signal %v: terminating\n", sig)
			run = false
		default:
			msg, err := consumer.ReadMessage(500 * time.Millisecond)
			if err == nil {
				if !shouldProcessMessage(msg, allowedRepos) {
					continue
				}

				if err := sendMessageAsHTTPPost(msg); err != nil {
					log.Printf("Failed to send message as HTTP POST: %s", err)
				}

			} else if err.(kafka.Error).Code() != kafka.ErrTimedOut {
				fmt.Printf("Consumer error: %v (%v)\n", err, msg)
			}
		}
	}

	log.Println("Closing consumer...")
}

func buildAllowedReposSet() map[string]bool {
	repoFilters := os.Getenv("REPO_FILTERS")
	if repoFilters == "" {
		return nil
	}

	allowed := make(map[string]bool)
	for _, r := range strings.Split(repoFilters, ",") {
		trimmed := strings.TrimSpace(r)
		if trimmed != "" {
			allowed[trimmed] = true
		}
	}
	return allowed
}

func shouldProcessMessage(msg *kafka.Message, allowedRepos map[string]bool) bool {
	if allowedRepos == nil {
		return true
	}

	keyParts := strings.Split(string(msg.Key), ".")
	if len(keyParts) < 2 {
		return false
	}
	orgRepo := keyParts[0] + "/" + keyParts[1]

	if allowedRepos[orgRepo] {
		return true
	}
	log.Printf("Skipping message from repo %s, not in filter", orgRepo)
	return false
}

func sendMessageAsHTTPPost(message *kafka.Message) error {
	endpoints := strings.Split(os.Getenv("REPLAY_ENDPOINTS"), ",")
	if len(endpoints) == 0 || (len(endpoints) == 1 && endpoints[0] == "") {
		return fmt.Errorf("REPLAY_ENDPOINTS not set")
	}

	for _, url := range endpoints {
		url = strings.TrimSpace(url)
		go func(endpoint string) {
			if err := sendToEndpoint(endpoint, message); err != nil {
				log.Printf("Failed to send to endpoint %s: %v", endpoint, err)
			} else {
				log.Printf("Successfully sent to endpoint %s", endpoint)
			}
		}(url)
	}

	return nil
}

func sendToEndpoint(endpoint string, message *kafka.Message) error {
	req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer(message.Value))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	for _, header := range message.Headers {
		key := string(header.Key)
		value := string(header.Value)
		req.Header.Set(key, value)
	}
	req.Header.Set("g2krepeater", "true")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("received non-2xx response: %d", resp.StatusCode)
	}

	log.Printf("Successfully sent HTTP POST request. Response: %d", resp.StatusCode)

	return nil
}

func LoadConfig() kafka.ConfigMap {
	m := make(map[string]kafka.ConfigValue)

	envVars := os.Environ()
	prefix := "KAFKA_"

	for _, envVar := range envVars {
		parts := strings.SplitN(envVar, "=", 2)
		key := parts[0]
		value := parts[1]

		if strings.HasPrefix(key, prefix) {
			key := strings.Replace(key, prefix, "", 1)
			kConfig := strings.ReplaceAll(strings.ToLower(key), "_", ".")
			m[kConfig] = value
			log.Printf("Env %s Value %s", kConfig, value)
		}
	}

	return m
}
