package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"go.uber.org/zap"
)

var isTopicCreated bool
var logger *zap.Logger

// WebhookPayload represents the structure of the GitHub webhook payload kinda hacky can be improved
type WebhookPayload struct {
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func main() {
	var err error
	logger, err = zap.NewProduction()
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
	defer logger.Sync()

	http.HandleFunc("/api/webhooks/github", webhookHandler)

	serverPort := os.Getenv("PORT")
	if serverPort == "" {
		serverPort = "5050"
	}

	logger.Info("Starting g2krelay server", zap.String("port", serverPort))

	if err := http.ListenAndServe(":"+serverPort, nil); err != nil {
		logger.Fatal("Server failed", zap.Error(err))
	}
}

func webhookHandler(w http.ResponseWriter, r *http.Request) {
	topic := "github.events"

	body, err := io.ReadAll(io.Reader(r.Body))
	if err != nil {
		http.Error(w, "Cannot read request body", http.StatusBadRequest)
		return
	}

	// Should I force webhook secret validation? -> probably
	if !validateSignature(r, body) {
		http.Error(w, "Invalid signature, check webhook secret", http.StatusUnauthorized)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	if eventType == "" {
		http.Error(w, "Event type not found", http.StatusBadRequest)
		return
	}

	var payload WebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	repoFullName := strings.ReplaceAll(payload.Repository.FullName, "/", ".")
	if repoFullName == "" {
		http.Error(w, "Repository full name not found", http.StatusBadRequest)
		return
	}
	messageKey := repoFullName + "." + eventType

	headers := getKafkaHeaders(r)
	if err := produceToKafka(topic, body, messageKey, headers); err != nil {
		logger.Error("Failed to produce message to Kafka",
			zap.Error(err),
			zap.String("topic", topic),
			zap.String("messageKey", messageKey))
		http.Error(w, "Failed to process webhook", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Webhook received and processed by g2krelay")
}

func getKafkaHeaders(r *http.Request) []kafka.Header {
	var headers []kafka.Header
	for name, values := range r.Header {
		for _, value := range values {
			header := kafka.Header{
				Key:   name,
				Value: []byte(value),
			}
			headers = append(headers, header)
		}
	}
	return headers
}

func validateSignature(r *http.Request, body []byte) bool {
	signature := r.Header.Get("X-Hub-Signature-256")
	if signature == "" {
		logger.Warn("No signature provided")
		return false
	}

	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		logger.Warn("WEBHOOK_SECRET not set")
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expectedSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expectedSignature), []byte(signature))
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
			logger.Debug("Kafka config loaded",
				zap.String("key", kConfig),
				zap.String("value", value))
		}
	}

	return m
}

func produceToKafka(topic string, message []byte, action string, headers []kafka.Header) error {
	config := LoadConfig()

	p, err := kafka.NewProducer(&config)
	if err != nil {
		return fmt.Errorf("failed to create producer: %w", err)
	}
	defer p.Close()

	if err := ensureTopicExists(topic, config); err != nil {
		return err
	}

	deliveryChan := make(chan kafka.Event, 1)

	err = p.Produce(&kafka.Message{
		TopicPartition: kafka.TopicPartition{Topic: &topic, Partition: kafka.PartitionAny},
		Key:            []byte(action),
		Value:          message,
		Headers:        headers,
	}, deliveryChan)
	if err != nil {
		return fmt.Errorf("failed to produce message: %w", err)
	}

	e := <-deliveryChan
	m := e.(*kafka.Message)
	if m.TopicPartition.Error != nil {
		return fmt.Errorf("delivery failed: %w", m.TopicPartition.Error)
	}

	logger.Info("Message delivered to Kafka",
		zap.String("topic", *m.TopicPartition.Topic),
		zap.Int32("partition", m.TopicPartition.Partition),
		zap.String("offset", m.TopicPartition.Offset.String()))
	close(deliveryChan)
	return nil
}

func ensureTopicExists(topic string, config kafka.ConfigMap) error {
	if isTopicCreated {
		return nil
	}

	partitionsStr := os.Getenv("TOPIC_PARTITIONS")
	if partitionsStr == "" {
		partitionsStr = "1"
	}

	numPartitions, err := strconv.Atoi(partitionsStr)
	if err != nil {
		logger.Warn("TOPIC_PARTITIONS not set, defaulting to 1",
			zap.Error(err))
		numPartitions = 1
	}

	adminClient, err := kafka.NewAdminClient(&config)
	if err != nil {
		return fmt.Errorf("failed to create admin client: %w", err)
	}
	defer adminClient.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	results, err := adminClient.CreateTopics(
		ctx,
		[]kafka.TopicSpecification{{
			Topic:             topic,
			NumPartitions:     numPartitions,
			ReplicationFactor: 1,
		}},
	)
	if err != nil {
		logger.Error("Failed to create topic",
			zap.String("topic", topic),
			zap.Error(err))
		return fmt.Errorf("failed to create topic: %w", err)
	}

	for _, result := range results {
		if result.Error.Code() != kafka.ErrNoError && result.Error.Code() != kafka.ErrTopicAlreadyExists {
			logger.Error("Topic creation error",
				zap.String("topic", result.Topic),
				zap.Error(result.Error))
			return fmt.Errorf("failed to create topic %s: %v", result.Topic, result.Error)
		}
		logger.Info("Topic status",
			zap.String("topic", result.Topic),
			zap.String("status", result.Error.String()))
	}

	isTopicCreated = true
	return nil
}
