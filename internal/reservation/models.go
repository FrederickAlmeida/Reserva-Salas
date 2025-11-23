package reservation

import (
	"errors"
	"time"
)

type Clock interface {
	Now() time.Time
}

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

const (
	slotMinutes = 30
	slotsPerDay = 24 * 60 / slotMinutes // 48
)

type ReservationStatus int

const (
	StatusPendenteConfirmacao ReservationStatus = iota + 1
	StatusConfirmada
	StatusCancelada
	StatusExpirada
)

type Reservation struct {
	ID        string
	RoomID    string
	Date      string // "YYYY-MM-DD"
	StartTime string // "HH:MM"
	EndTime   string // "HH:MM"
	Status    ReservationStatus
	CreatedAt time.Time
}

type TimeSlot struct {
	StartTime string
	EndTime   string
}

var (
	ErrRoomNotFound        = errors.New("room not found")
	ErrReservationNotFound = errors.New("reservation not found")
	ErrTimeConflict        = errors.New("time conflict")
	ErrInvalidTimeRange    = errors.New("invalid time range")
	ErrAlreadyFinalized    = errors.New("reservation already finalized")
	ErrExpired             = errors.New("reservation expired")
)
