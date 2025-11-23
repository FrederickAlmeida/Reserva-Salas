package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/FrederickAlmeida/Reserva-Salas/internal/mq"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: mq-client [list|reserve|cancel|confirm] [flags]")
		os.Exit(1)
	}

	cmd := os.Args[1]

	amqpURL := os.Getenv("AMQP_URL")
	if amqpURL == "" {
		amqpURL = "amqp://guest:guest@localhost:5672/"
	}

	queueName := os.Getenv("BOOKING_QUEUE")
	if queueName == "" {
		queueName = "booking.commands"
	}

	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		log.Fatalf("failed to connect to RabbitMQ: %v", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		log.Fatalf("failed to open channel: %v", err)
	}
	defer ch.Close()

	// fila de resposta temporária
	replyQueue, err := ch.QueueDeclare(
		"",
		false,
		true,  // auto-delete
		true,  // exclusive
		false, // no-wait
		nil,
	)
	if err != nil {
		log.Fatalf("failed to declare reply queue: %v", err)
	}

	replies, err := ch.Consume(
		replyQueue.Name,
		"",
		true,  // auto-ack
		true,  // exclusive
		false,
		false,
		nil,
	)
	if err != nil {
		log.Fatalf("failed to register reply consumer: %v", err)
	}

	switch cmd {
	case "list":
		listCmd(ch, replies, queueName, replyQueue.Name, os.Args[2:])
	case "reserve":
		reserveCmd(ch, replies, queueName, replyQueue.Name, os.Args[2:])
	case "cancel":
		cancelCmd(ch, replies, queueName, replyQueue.Name, os.Args[2:])
	case "confirm":
		confirmCmd(ch, replies, queueName, replyQueue.Name, os.Args[2:])
	default:
		fmt.Println("unknown command:", cmd)
		os.Exit(1)
	}
}

// ----- Helpers gerais -----

func sendCommandAndWait(ch *amqp.Channel, replies <-chan amqp.Delivery, queueName, replyTo string, env mq.CommandEnvelope) (mq.Response, error) {
	var zero mq.Response

	body, err := json.Marshal(env)
	if err != nil {
		return zero, fmt.Errorf("failed to marshal envelope: %w", err)
	}

	correlationID := fmt.Sprintf("%d", time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = ch.PublishWithContext(
		ctx,
		"",
		queueName,
		false,
		false,
		amqp.Publishing{
			ContentType:   "application/json",
			ReplyTo:       replyTo,
			CorrelationId: correlationID,
			Body:          body,
		},
	)
	if err != nil {
		return zero, fmt.Errorf("failed to publish command: %w", err)
	}

	for {
		select {
		case msg := <-replies:
			if msg.CorrelationId != correlationID {
				// outra resposta (teoricamente não deveria acontecer neste cliente)
				continue
			}
			var resp mq.Response
			if err := json.Unmarshal(msg.Body, &resp); err != nil {
				return zero, fmt.Errorf("failed to unmarshal response: %w", err)
			}
			return resp, nil
		case <-ctx.Done():
			return zero, fmt.Errorf("timeout waiting for response")
		}
	}
}

// ----- Comandos específicos -----

func listCmd(ch *amqp.Channel, replies <-chan amqp.Delivery, queueName, replyTo string, args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	date := fs.String("date", "", "date (YYYY-MM-DD)")
	_ = fs.Parse(args)

	if *date == "" {
		log.Fatal("date is required")
	}

	reqPayload := mq.ListAvailablePayload{Date: *date}
	payloadBytes, _ := json.Marshal(reqPayload)

	env := mq.CommandEnvelope{
		Type:    mq.CommandListAvailable,
		Payload: payloadBytes,
	}

	resp, err := sendCommandAndWait(ch, replies, queueName, replyTo, env)
	if err != nil {
		log.Fatalf("ListAvailable error: %v", err)
	}
	if !resp.OK {
		log.Fatalf("ListAvailable error: %s", resp.Error)
	}

	var payload mq.ListAvailableResponsePayload
	if err := json.Unmarshal(resp.Payload, &payload); err != nil {
		log.Fatalf("failed to decode response payload: %v", err)
	}

	fmt.Printf("Salas livres em %s:\n", *date)
	for _, room := range payload.Rooms {
		fmt.Printf("- %s:\n", room.RoomID)
		if len(room.FreeSlots) == 0 {
			fmt.Println("  (sem horários livres)")
			continue
		}
		for _, slot := range room.FreeSlots {
			fmt.Printf("  %s - %s\n", slot.StartTime, slot.EndTime)
		}
	}
}

func reserveCmd(ch *amqp.Channel, replies <-chan amqp.Delivery, queueName, replyTo string, args []string) {
	fs := flag.NewFlagSet("reserve", flag.ExitOnError)
	room := fs.String("room", "", "room id (ex: sala-101)")
	date := fs.String("date", "", "date (YYYY-MM-DD)")
	start := fs.String("start", "", "start time (HH:MM)")
	end := fs.String("end", "", "end time (HH:MM)")
	_ = fs.Parse(args)

	if *room == "" || *date == "" || *start == "" || *end == "" {
		log.Fatal("room, date, start and end are required")
	}

	reqPayload := mq.CreateReservationPayload{
		RoomID:    *room,
		Date:      *date,
		StartTime: *start,
		EndTime:   *end,
	}
	payloadBytes, _ := json.Marshal(reqPayload)

	env := mq.CommandEnvelope{
		Type:    mq.CommandCreateReservation,
		Payload: payloadBytes,
	}

	resp, err := sendCommandAndWait(ch, replies, queueName, replyTo, env)
	if err != nil {
		log.Fatalf("CreateReservation error: %v", err)
	}
	if !resp.OK {
		log.Fatalf("CreateReservation error: %s", resp.Error)
	}

	var payload mq.CreateReservationResponsePayload
	if err := json.Unmarshal(resp.Payload, &payload); err != nil {
		log.Fatalf("failed to decode response payload: %v", err)
	}

	fmt.Printf("Reserva criada. ID: %s | Status: %s | Msg: %s\n",
		payload.ReservationID, payload.Status, payload.Message)
}

func cancelCmd(ch *amqp.Channel, replies <-chan amqp.Delivery, queueName, replyTo string, args []string) {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	id := fs.String("id", "", "reservation id")
	_ = fs.Parse(args)

	if *id == "" {
		log.Fatal("id is required")
	}

	reqPayload := mq.CancelReservationPayload{
		ReservationID: *id,
	}
	payloadBytes, _ := json.Marshal(reqPayload)

	env := mq.CommandEnvelope{
		Type:    mq.CommandCancelReservation,
		Payload: payloadBytes,
	}

	resp, err := sendCommandAndWait(ch, replies, queueName, replyTo, env)
	if err != nil {
		log.Fatalf("CancelReservation error: %v", err)
	}
	if !resp.OK {
		log.Fatalf("CancelReservation error: %s", resp.Error)
	}

	var payload mq.CancelReservationResponsePayload
	if err := json.Unmarshal(resp.Payload, &payload); err != nil {
		log.Fatalf("failed to decode response payload: %v", err)
	}

	fmt.Printf("Cancelamento: Status: %s | Msg: %s\n",
		payload.Status, payload.Message)
}

func confirmCmd(ch *amqp.Channel, replies <-chan amqp.Delivery, queueName, replyTo string, args []string) {
	fs := flag.NewFlagSet("confirm", flag.ExitOnError)
	id := fs.String("id", "", "reservation id")
	_ = fs.Parse(args)

	if *id == "" {
		log.Fatal("id is required")
	}

	reqPayload := mq.ConfirmReservationPayload{
		ReservationID: *id,
	}
	payloadBytes, _ := json.Marshal(reqPayload)

	env := mq.CommandEnvelope{
		Type:    mq.CommandConfirmReservation,
		Payload: payloadBytes,
	}

	resp, err := sendCommandAndWait(ch, replies, queueName, replyTo, env)
	if err != nil {
		log.Fatalf("ConfirmReservation error: %v", err)
	}
	if !resp.OK {
		log.Fatalf("ConfirmReservation error: %s", resp.Error)
	}

	var payload mq.ConfirmReservationResponsePayload
	if err := json.Unmarshal(resp.Payload, &payload); err != nil {
		log.Fatalf("failed to decode response payload: %v", err)
	}

	fmt.Printf("Confirmação: Status: %s | Msg: %s\n",
		payload.Status, payload.Message)
}
