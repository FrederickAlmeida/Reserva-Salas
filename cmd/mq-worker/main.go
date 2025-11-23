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
	amqpURL := os.Getenv("AMQP_URL")
	if amqpURL == "" {
		amqpURL = "amqp://guest:guest@localhost:5672/"
	}

	queueName := os.Getenv("BOOKING_QUEUE")
	if queueName == "" {
		queueName = "booking.commands"
	}

	// domínio (mesma Agenda de antes)
	agenda := reservation.NewAgenda(reservation.RealClock{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// worker de expiração de reservas (igual gRPC)
	agenda.StartExpirationWorker(ctx, time.Minute)

	// conexão com RabbitMQ
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

	_, err = ch.QueueDeclare(
		queueName,
		true,  // durable
		false, // auto-delete
		false, // exclusive
		false, // no-wait
		nil,
	)
	if err != nil {
		log.Fatalf("failed to declare queue: %v", err)
	}

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
		log.Fatalf("failed to register consumer: %v", err)
	}

	log.Printf("MQ worker listening on queue %s", queueName)

	// captura sinais para shutdown gracioso
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		for d := range msgs {
			handleDelivery(ctx, &d, ch, agenda)
		}
	}()

	<-stopCh
	log.Println("shutting down mq-worker...")
	cancel()
	time.Sleep(500 * time.Millisecond)
	log.Println("mq-worker stopped")
}

func handleDelivery(parentCtx context.Context, d *amqp.Delivery, ch *amqp.Channel, agenda *reservation.Agenda) {
	defer func() {
		// sempre dá ack pra não ficar reentregando infinitamente
		if err := d.Ack(false); err != nil {
			log.Printf("failed to ack message: %v", err)
		}
	}()

	var env mq.CommandEnvelope
	if err := json.Unmarshal(d.Body, &env); err != nil {
		log.Printf("invalid command envelope: %v", err)
		sendErrorResponse(parentCtx, ch, d, "invalid command format: "+err.Error())
		return
	}

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
		log.Printf("unknown command type: %s", env.Type)
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
		log.Printf("failed to marshal response: %v", err)
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
		log.Printf("failed to publish response: %v", err)
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
		sendErrorResponse(ctx, ch, d, "invalid payload: "+err.Error())
		return
	}
	if req.Date == "" {
		sendErrorResponse(ctx, ch, d, "date is required")
		return
	}

	avail, err := agenda.ListAvailable(ctx, req.Date)
	if err != nil {
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
	sendResponse(ctx, ch, d, resp)
}

func handleCreateReservation(ctx context.Context, payload json.RawMessage, ch *amqp.Channel, d *amqp.Delivery, agenda *reservation.Agenda) {
	var req mq.CreateReservationPayload
	if err := json.Unmarshal(payload, &req); err != nil {
		sendErrorResponse(ctx, ch, d, "invalid payload: "+err.Error())
		return
	}
	if req.RoomID == "" || req.Date == "" || req.StartTime == "" || req.EndTime == "" {
		sendErrorResponse(ctx, ch, d, "room_id, date, start_time and end_time are required")
		return
	}

	res, err := agenda.CreateReservation(ctx, req.RoomID, req.Date, req.StartTime, req.EndTime)
	if err != nil {
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
	sendResponse(ctx, ch, d, resp)
}

func handleCancelReservation(ctx context.Context, payload json.RawMessage, ch *amqp.Channel, d *amqp.Delivery, agenda *reservation.Agenda) {
	var req mq.CancelReservationPayload
	if err := json.Unmarshal(payload, &req); err != nil {
		sendErrorResponse(ctx, ch, d, "invalid payload: "+err.Error())
		return
	}
	if req.ReservationID == "" {
		sendErrorResponse(ctx, ch, d, "reservation_id is required")
		return
	}

	res, err := agenda.CancelReservation(ctx, req.ReservationID)
	if err != nil {
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
	sendResponse(ctx, ch, d, resp)
}

func handleConfirmReservation(ctx context.Context, payload json.RawMessage, ch *amqp.Channel, d *amqp.Delivery, agenda *reservation.Agenda) {
	var req mq.ConfirmReservationPayload
	if err := json.Unmarshal(payload, &req); err != nil {
		sendErrorResponse(ctx, ch, d, "invalid payload: "+err.Error())
		return
	}
	if req.ReservationID == "" {
		sendErrorResponse(ctx, ch, d, "reservation_id is required")
		return
	}

	res, err := agenda.ConfirmReservation(ctx, req.ReservationID)
	if err != nil {
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
