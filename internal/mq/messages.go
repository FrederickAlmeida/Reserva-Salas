package mq

import "encoding/json"

// Tipo de comando enviado via RabbitMQ
type CommandType string

const (
	CommandListAvailable      CommandType = "ListAvailable"
	CommandCreateReservation  CommandType = "CreateReservation"
	CommandCancelReservation  CommandType = "CancelReservation"
	CommandConfirmReservation CommandType = "ConfirmReservation"
)

// Envelope genérico de comando
type CommandEnvelope struct {
	Type    CommandType     `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Payloads de requisição

type ListAvailablePayload struct {
	Date string `json:"date"`
}

type CreateReservationPayload struct {
	RoomID    string `json:"room_id"`
	Date      string `json:"date"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
}

type CancelReservationPayload struct {
	ReservationID string `json:"reservation_id"`
}

type ConfirmReservationPayload struct {
	ReservationID string `json:"reservation_id"`
}

// Estruturas de resposta

type TimeSlot struct {
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
}

type RoomAvailability struct {
	RoomID    string     `json:"room_id"`
	FreeSlots []TimeSlot `json:"free_slots"`
}

type ListAvailableResponsePayload struct {
	Rooms []RoomAvailability `json:"rooms"`
}

type CreateReservationResponsePayload struct {
	ReservationID string `json:"reservation_id"`
	Status        string `json:"status"`
	Message       string `json:"message"`
}

type CancelReservationResponsePayload struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type ConfirmReservationResponsePayload struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// Envelope genérico de resposta

type Response struct {
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}
