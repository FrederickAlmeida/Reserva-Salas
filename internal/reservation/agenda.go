package reservation

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Agenda struct {
	mu    sync.RWMutex
	byDate map[string]map[string][]*Reservation // date -> room -> reservas
	byID   map[string]*Reservation
	rooms  []string
	clock  Clock
}

func NewAgenda(clock Clock) *Agenda {
	return &Agenda{
		byDate: make(map[string]map[string][]*Reservation),
		byID:   make(map[string]*Reservation),
		rooms:  defaultRooms(),
		clock:  clock,
	}
}

func defaultRooms() []string {
	rooms := make([]string, 10)
	for i := 0; i < 10; i++ {
		rooms[i] = fmt.Sprintf("sala-%03d", 101+i)
	}
	return rooms
}

func (a *Agenda) Rooms() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, len(a.rooms))
	copy(out, a.rooms)
	return out
}

func (a *Agenda) roomExists(roomID string) bool {
	for _, r := range a.rooms {
		if r == roomID {
			return true
		}
	}
	return false
}

func buildReservationID(roomID, date, start, end string) string {
	return fmt.Sprintf("%s_%s_%s_%s", roomID, date, start, end)
}

func parseTimeOfDay(s string) (int, error) {
	// s = "HH:MM"
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, err
	}
	minutes := t.Hour()*60 + t.Minute()
	if minutes%slotMinutes != 0 {
		return 0, fmt.Errorf("time %s not aligned to %d minutes", s, slotMinutes)
	}
	return minutes, nil
}

func formatTimeOfDay(minutes int) string {
	h := minutes / 60
	m := minutes % 60
	return fmt.Sprintf("%02d:%02d", h, m)
}

// CreateReservation cria uma reserva, garantindo ausência de conflito.
func (a *Agenda) CreateReservation(ctx context.Context, roomID, date, startTime, endTime string) (*Reservation, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	startMin, err := parseTimeOfDay(startTime)
	if err != nil {
		return nil, ErrInvalidTimeRange
	}
	endMin, err := parseTimeOfDay(endTime)
	if err != nil {
		return nil, ErrInvalidTimeRange
	}
	if endMin <= startMin {
		return nil, ErrInvalidTimeRange
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.roomExists(roomID) {
		return nil, ErrRoomNotFound
	}

	if _, ok := a.byDate[date]; !ok {
		a.byDate[date] = make(map[string][]*Reservation)
	}
	if _, ok := a.byDate[date][roomID]; !ok {
		a.byDate[date][roomID] = []*Reservation{}
	}

	// Checa conflitos com reservas existentes (Pendente + Confirmada)
	for _, existing := range a.byDate[date][roomID] {
		if existing.Status == StatusCancelada || existing.Status == StatusExpirada {
			continue
		}
		exStart, _ := parseTimeOfDay(existing.StartTime)
		exEnd, _ := parseTimeOfDay(existing.EndTime)
		// conflito se intervalos se sobrepõem
		if !(endMin <= exStart || startMin >= exEnd) {
			return nil, ErrTimeConflict
		}
	}

	now := a.clock.Now()
	res := &Reservation{
		ID:        buildReservationID(roomID, date, startTime, endTime),
		RoomID:    roomID,
		Date:      date,
		StartTime: startTime,
		EndTime:   endTime,
		Status:    StatusPendenteConfirmacao,
		CreatedAt: now,
	}

	a.byDate[date][roomID] = append(a.byDate[date][roomID], res)
	a.byID[res.ID] = res

	return res, nil
}

func (a *Agenda) findReservationLocked(id string) (*Reservation, error) {
	res, ok := a.byID[id]
	if !ok {
		return nil, ErrReservationNotFound
	}
	return res, nil
}

// CancelReservation cancela uma reserva, se ainda não estiver finalizada.
func (a *Agenda) CancelReservation(ctx context.Context, id string) (*Reservation, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	res, err := a.findReservationLocked(id)
	if err != nil {
		return nil, err
	}

	if res.Status == StatusCancelada || res.Status == StatusExpirada {
		return nil, ErrAlreadyFinalized
	}

	res.Status = StatusCancelada
	return res, nil
}

// ConfirmReservation confirma se ainda estiver pendente e não expirada.
func (a *Agenda) ConfirmReservation(ctx context.Context, id string) (*Reservation, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	res, err := a.findReservationLocked(id)
	if err != nil {
		return nil, err
	}

	now := a.clock.Now()
	loc := now.Location()
	dateD, err := time.ParseInLocation("2006-01-02", res.Date, loc)
	if err != nil {
		return nil, ErrInvalidTimeRange
	}

	// Se já passou da meia noite de D, está expirada
	if !now.Before(dateD) && res.Status == StatusPendenteConfirmacao {
		res.Status = StatusExpirada
		return nil, ErrExpired
	}

	if res.Status == StatusConfirmada {
		return res, nil
	}
	if res.Status == StatusCancelada || res.Status == StatusExpirada {
		return nil, ErrAlreadyFinalized
	}

	res.Status = StatusConfirmada
	return res, nil
}

// ListAvailable retorna, para a data, os intervalos livres de cada sala.
func (a *Agenda) ListAvailable(ctx context.Context, date string) (map[string][]TimeSlot, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make(map[string][]TimeSlot)

	for _, room := range a.rooms {
		free := make([]bool, slotsPerDay)
		for i := range free {
			free[i] = true
		}

		roomRes := a.byDate[date][room]

		for _, res := range roomRes {
			if res.Status == StatusCancelada || res.Status == StatusExpirada {
				continue
			}
			startMin, _ := parseTimeOfDay(res.StartTime)
			endMin, _ := parseTimeOfDay(res.EndTime)
			startIdx := startMin / slotMinutes
			endIdx := endMin / slotMinutes
			if startIdx < 0 {
				startIdx = 0
			}
			if endIdx > slotsPerDay {
				endIdx = slotsPerDay
			}
			for i := startIdx; i < endIdx; i++ {
				free[i] = false
			}
		}

		// Converte slots livres em TimeSlot
		var slots []TimeSlot
		i := 0
		for i < slotsPerDay {
			if !free[i] {
				i++
				continue
			}
			startIdx := i
			for i < slotsPerDay && free[i] {
				i++
			}
			endIdx := i
			startMin := startIdx * slotMinutes
			endMin := endIdx * slotMinutes
			slots = append(slots, TimeSlot{
				StartTime: formatTimeOfDay(startMin),
				EndTime:   formatTimeOfDay(endMin),
			})
		}
		result[room] = slots
	}

	return result, nil
}

// StartExpirationWorker roda em background expirando reservas pendentes.
func (a *Agenda) StartExpirationWorker(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.expirePendingReservations()
			}
		}
	}()
}

func (a *Agenda) expirePendingReservations() {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.clock.Now()
	loc := now.Location()

	for _, res := range a.byID {
		if res.Status != StatusPendenteConfirmacao {
			continue
		}
		dateD, err := time.ParseInLocation("2006-01-02", res.Date, loc)
		if err != nil {
			continue
		}
		if !now.Before(dateD) {
			res.Status = StatusExpirada
		}
	}
}
