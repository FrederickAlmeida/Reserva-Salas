package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/FrederickAlmeida/Reserva-Salas/internal/mq"
	"github.com/FrederickAlmeida/Reserva-Salas/internal/reservation"
)

func main() {
	log.Println("[MQ] Initializing RabbitMQ worker...")
	amqpURL := os.Getenv("AMQP_URL")
	if amqpURL == "" {
		amqpURL = "amqp://guest:guest@localhost:5672/"
	}

	queueName := os.Getenv("BOOKING_QUEUE")
	if queueName == "" {
		queueName = "booking.commands"
	}

	log.Printf("[MQ] Connecting to RabbitMQ at %s", amqpURL)
	log.Printf("[MQ] Queue name: %s", queueName)

	// domínio
	agenda := reservation.NewAgenda(reservation.RealClock{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// worker de expiração de reservas
	log.Println("[MQ] Starting expiration worker...")
	agenda.StartExpirationWorker(ctx, time.Minute)

	// conexão com RabbitMQ
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		log.Fatalf("[MQ] failed to connect to RabbitMQ: %v", err)
	}
	defer conn.Close()
	log.Println("[MQ] Connected to RabbitMQ")

	ch, err := conn.Channel()
	if err != nil {
		log.Fatalf("[MQ] failed to open channel: %v", err)
	}
	defer ch.Close()
	log.Println("[MQ] Channel opened")

	_, err = ch.QueueDeclare(
		queueName,
		true,  // durable
		false, // auto-delete
		false, // exclusive
		false, // no-wait
		nil,
	)
	if err != nil {
		log.Fatalf("[MQ] failed to declare queue: %v", err)
	}
	log.Printf("[MQ] Queue declared: %s", queueName)

	msgs, err := ch.Consume(
		queueName,
		"",
		false, // auto-ack = false (vamos dar ack manualmente)
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		log.Fatalf("[MQ] failed to register consumer: %v", err)
	}

	log.Printf("[MQ] Worker listening on queue %s", queueName)

	// captura sinais para shutdown gracioso
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		for d := range msgs {
			handleDelivery(ctx, &d, ch, agenda)
		}
	}()

	<-stopCh
	log.Println("[MQ] Shutting down worker...")
	cancel()
	time.Sleep(500 * time.Millisecond)
	log.Println("[MQ] Worker stopped")
}

func handleDelivery(parentCtx context.Context, d *amqp.Delivery, ch *amqp.Channel, agenda *reservation.Agenda) {
	defer func() {
		// sempre dá ack pra não ficar reentregando infinitamente
		if err := d.Ack(false); err != nil {
			log.Printf("[MQ] failed to ack message: %v", err)
		}
	}()

	var env mq.CommandEnvelope
	if err := json.Unmarshal(d.Body, &env); err != nil {
		log.Printf("[MQ] invalid command envelope: %v", err)
		sendErrorResponse(parentCtx, ch, d, "invalid command format: "+err.Error())
		return
	}

	log.Printf("[MQ] Received command: %s", env.Type)

	ctx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancel()

	switch env.Type {
	case mq.CommandListAvailable:
		handleListAvailable(ctx, env.Payload, ch, d, agenda)
	case mq.CommandCreateReservation:
		handleCreateReservation(ctx, env.Payload, ch, d, agenda)
	case mq.CommandCancelReservation:
		handleCancelReservation(ctx, env.Payload, ch, d, agenda)
	case mq.CommandConfirmReservation:
		handleConfirmReservation(ctx, env.Payload, ch, d, agenda)
	default:
		log.Printf("[MQ] unknown command type: %s", env.Type)
		sendErrorResponse(ctx, ch, d, "unknown command type: "+string(env.Type))
	}
}

func sendResponse(ctx context.Context, ch *amqp.Channel, d *amqp.Delivery, resp mq.Response) {
	if d.ReplyTo == "" {
		// "fire-and-forget"
		return
	}
	body, err := json.Marshal(resp)
	if err != nil {
		log.Printf("[MQ] failed to marshal response: %v", err)
		return
	}

	err = ch.PublishWithContext(
		ctx,
		"",
		d.ReplyTo,
		false,
		false,
		amqp.Publishing{
			ContentType:   "application/json",
			CorrelationId: d.CorrelationId,
			Body:          body,
		},
	)
	if err != nil {
		log.Printf("[MQ] failed to publish response: %v", err)
	} else {
		log.Printf("[MQ] Response sent - type: %s, ok: %v", resp.Type, resp.OK)
	}
}

func sendErrorResponse(ctx context.Context, ch *amqp.Channel, d *amqp.Delivery, message string) {
	resp := mq.Response{
		OK:    false,
		Error: message,
		Type:  "Error",
	}
	sendResponse(ctx, ch, d, resp)
}

// ===== handlers de comandos =====

func handleListAvailable(ctx context.Context, payload json.RawMessage, ch *amqp.Channel, d *amqp.Delivery, agenda *reservation.Agenda) {
	var req mq.ListAvailablePayload
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Printf("[MQ] ListAvailable - invalid payload: %v", err)
		sendErrorResponse(ctx, ch, d, "invalid payload: "+err.Error())
		return
	}
	if req.Date == "" {
		log.Printf("[MQ] ListAvailable - date is required")
		sendErrorResponse(ctx, ch, d, "date is required")
		return
	}

	log.Printf("[MQ] ListAvailable - date: %s", req.Date)
	avail, err := agenda.ListAvailable(ctx, req.Date)
	if err != nil {
		log.Printf("[MQ] ListAvailable error: %v", err)
		sendErrorResponse(ctx, ch, d, err.Error())
		return
	}

	var rooms []mq.RoomAvailability
	for roomID, slots := range avail {
		ra := mq.RoomAvailability{
			RoomID:    roomID,
			FreeSlots: make([]mq.TimeSlot, 0, len(slots)),
		}
		for _, sl := range slots {
			ra.FreeSlots = append(ra.FreeSlots, mq.TimeSlot{
				StartTime: sl.StartTime,
				EndTime:   sl.EndTime,
			})
		}
		rooms = append(rooms, ra)
	}

	respPayload := mq.ListAvailableResponsePayload{Rooms: rooms}
	payloadBytes, _ := json.Marshal(respPayload)

	resp := mq.Response{
		OK:      true,
		Type:    "ListAvailableResponse",
		Payload: payloadBytes,
	}
	log.Printf("[MQ] ListAvailable success - found %d rooms", len(rooms))
	sendResponse(ctx, ch, d, resp)
}

func handleCreateReservation(ctx context.Context, payload json.RawMessage, ch *amqp.Channel, d *amqp.Delivery, agenda *reservation.Agenda) {
	var req mq.CreateReservationPayload
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Printf("[MQ] CreateReservation - invalid payload: %v", err)
		sendErrorResponse(ctx, ch, d, "invalid payload: "+err.Error())
		return
	}
	if req.RoomID == "" || req.Date == "" || req.StartTime == "" || req.EndTime == "" {
		log.Printf("[MQ] CreateReservation - missing required fields")
		sendErrorResponse(ctx, ch, d, "room_id, date, start_time and end_time are required")
		return
	}

	log.Printf("[MQ] CreateReservation - room: %s, date: %s, time: %s-%s", req.RoomID, req.Date, req.StartTime, req.EndTime)
	res, err := agenda.CreateReservation(ctx, req.RoomID, req.Date, req.StartTime, req.EndTime)
	if err != nil {
		log.Printf("[MQ] CreateReservation error: %v", err)
		sendErrorResponse(ctx, ch, d, err.Error())
		return
	}

	respPayload := mq.CreateReservationResponsePayload{
		ReservationID: res.ID,
		Status:        reservationStatusToString(res.Status),
		Message:       "reserva criada com sucesso",
	}
	payloadBytes, _ := json.Marshal(respPayload)

	resp := mq.Response{
		OK:      true,
		Type:    "CreateReservationResponse",
		Payload: payloadBytes,
	}
	log.Printf("[MQ] CreateReservation success - id: %s, status: %s", res.ID, reservationStatusToString(res.Status))
	sendResponse(ctx, ch, d, resp)
}

func handleCancelReservation(ctx context.Context, payload json.RawMessage, ch *amqp.Channel, d *amqp.Delivery, agenda *reservation.Agenda) {
	var req mq.CancelReservationPayload
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Printf("[MQ] CancelReservation - invalid payload: %v", err)
		sendErrorResponse(ctx, ch, d, "invalid payload: "+err.Error())
		return
	}
	if req.ReservationID == "" {
		log.Printf("[MQ] CancelReservation - reservation_id is required")
		sendErrorResponse(ctx, ch, d, "reservation_id is required")
		return
	}

	log.Printf("[MQ] CancelReservation - id: %s", req.ReservationID)
	res, err := agenda.CancelReservation(ctx, req.ReservationID)
	if err != nil {
		log.Printf("[MQ] CancelReservation error: %v", err)
		sendErrorResponse(ctx, ch, d, err.Error())
		return
	}

	respPayload := mq.CancelReservationResponsePayload{
		Status:  reservationStatusToString(res.Status),
		Message: "reserva cancelada",
	}
	payloadBytes, _ := json.Marshal(respPayload)

	resp := mq.Response{
		OK:      true,
		Type:    "CancelReservationResponse",
		Payload: payloadBytes,
	}
	log.Printf("[MQ] CancelReservation success - id: %s, status: %s", req.ReservationID, reservationStatusToString(res.Status))
	sendResponse(ctx, ch, d, resp)
}

func handleConfirmReservation(ctx context.Context, payload json.RawMessage, ch *amqp.Channel, d *amqp.Delivery, agenda *reservation.Agenda) {
	var req mq.ConfirmReservationPayload
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Printf("[MQ] ConfirmReservation - invalid payload: %v", err)
		sendErrorResponse(ctx, ch, d, "invalid payload: "+err.Error())
		return
	}
	if req.ReservationID == "" {
		log.Printf("[MQ] ConfirmReservation - reservation_id is required")
		sendErrorResponse(ctx, ch, d, "reservation_id is required")
		return
	}

	log.Printf("[MQ] ConfirmReservation - id: %s", req.ReservationID)
	res, err := agenda.ConfirmReservation(ctx, req.ReservationID)
	if err != nil {
		log.Printf("[MQ] ConfirmReservation error: %v", err)
		sendErrorResponse(ctx, ch, d, err.Error())
		return
	}

	respPayload := mq.ConfirmReservationResponsePayload{
		Status:  reservationStatusToString(res.Status),
		Message: "reserva confirmada",
	}
	payloadBytes, _ := json.Marshal(respPayload)

	resp := mq.Response{
		OK:      true,
		Type:    "ConfirmReservationResponse",
		Payload: payloadBytes,
	}
	log.Printf("[MQ] ConfirmReservation success - id: %s, status: %s", req.ReservationID, reservationStatusToString(res.Status))
	sendResponse(ctx, ch, d, resp)
}

func reservationStatusToString(st reservation.ReservationStatus) string {
	switch st {
	case reservation.StatusPendenteConfirmacao:
		return "PENDENTE_CONFIRMACAO"
	case reservation.StatusConfirmada:
		return "CONFIRMADA"
	case reservation.StatusCancelada:
		return "CANCELADA"
	case reservation.StatusExpirada:
		return "EXPIRADA"
	default:
		return "UNKNOWN"
	}
}
