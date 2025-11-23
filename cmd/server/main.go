package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/FrederickAlmeida/Reserva-Salas/internal/reservation"
	bookingpb "github.com/FrederickAlmeida/Reserva-Salas/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type reservationServer struct {
	bookingpb.UnimplementedReservationServiceServer
	agenda *reservation.Agenda
}

func (s *reservationServer) ListAvailable(ctx context.Context, req *bookingpb.ListAvailableRequest) (*bookingpb.ListAvailableResponse, error) {
	if req.GetDate() == "" {
		return nil, status.Error(codes.InvalidArgument, "date is required")
	}

	avail, err := s.agenda.ListAvailable(ctx, req.GetDate())
	if err != nil {
		return nil, statusFromDomainError(err)
	}

	resp := &bookingpb.ListAvailableResponse{}
	for roomID, slots := range avail {
		ra := &bookingpb.RoomAvailability{
			RoomId: roomID,
		}
		for _, sl := range slots {
			ra.FreeSlots = append(ra.FreeSlots, &bookingpb.TimeSlot{
				StartTime: sl.StartTime,
				EndTime:   sl.EndTime,
			})
		}
		resp.Rooms = append(resp.Rooms, ra)
	}
	return resp, nil
}

func (s *reservationServer) CreateReservation(ctx context.Context, req *bookingpb.CreateReservationRequest) (*bookingpb.CreateReservationResponse, error) {
	if req.GetRoomId() == "" || req.GetDate() == "" || req.GetStartTime() == "" || req.GetEndTime() == "" {
		return nil, status.Error(codes.InvalidArgument, "room_id, date, start_time and end_time are required")
	}

	res, err := s.agenda.CreateReservation(ctx, req.GetRoomId(), req.GetDate(), req.GetStartTime(), req.GetEndTime())
	if err != nil {
		return nil, statusFromDomainError(err)
	}

	return &bookingpb.CreateReservationResponse{
		ReservationId: res.ID,
		Status:        toProtoStatus(res.Status),
		Message:       "reserva criada com sucesso",
	}, nil
}

func (s *reservationServer) CancelReservation(ctx context.Context, req *bookingpb.CancelReservationRequest) (*bookingpb.CancelReservationResponse, error) {
	if req.GetReservationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "reservation_id is required")
	}

	res, err := s.agenda.CancelReservation(ctx, req.GetReservationId())
	if err != nil {
		return nil, statusFromDomainError(err)
	}

	return &bookingpb.CancelReservationResponse{
		Status:  toProtoStatus(res.Status),
		Message: "reserva cancelada",
	}, nil
}

func (s *reservationServer) ConfirmReservation(ctx context.Context, req *bookingpb.ConfirmReservationRequest) (*bookingpb.ConfirmReservationResponse, error) {
	if req.GetReservationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "reservation_id is required")
	}

	res, err := s.agenda.ConfirmReservation(ctx, req.GetReservationId())
	if err != nil {
		return nil, statusFromDomainError(err)
	}

	return &bookingpb.ConfirmReservationResponse{
		Status:  toProtoStatus(res.Status),
		Message: "reserva confirmada",
	}, nil
}

func toProtoStatus(st reservation.ReservationStatus) bookingpb.ReservationStatus {
	switch st {
	case reservation.StatusPendenteConfirmacao:
		return bookingpb.ReservationStatus_PENDENTE_CONFIRMACAO
	case reservation.StatusConfirmada:
		return bookingpb.ReservationStatus_CONFIRMADA
	case reservation.StatusCancelada:
		return bookingpb.ReservationStatus_CANCELADA
	case reservation.StatusExpirada:
		return bookingpb.ReservationStatus_EXPIRADA
	default:
		return bookingpb.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED
	}
}

func statusFromDomainError(err error) error {
	switch {
	case err == nil:
		return nil
	case errorsIs(err, reservation.ErrRoomNotFound),
		errorsIs(err, reservation.ErrReservationNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errorsIs(err, reservation.ErrTimeConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errorsIs(err, reservation.ErrInvalidTimeRange):
		return status.Error(codes.InvalidArgument, err.Error())
	case errorsIs(err, reservation.ErrExpired):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errorsIs(err, reservation.ErrAlreadyFinalized):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// helper pra evitar importar "errors" em todo lugar
func errorsIs(err, target error) bool {
	return err != nil && target != nil && err.Error() == target.Error()
}

func main() {
	addr := ":50051"
	if v := os.Getenv("BOOKING_ADDR"); v != "" {
		addr = v
	}

	agenda := reservation.NewAgenda(reservation.RealClock{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agenda.StartExpirationWorker(ctx, time.Minute)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	srv := &reservationServer{agenda: agenda}
	bookingpb.RegisterReservationServiceServer(grpcServer, srv)

	go func() {
		log.Printf("server listening at %s\n", addr)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("failed to serve: %v", err)
		}
	}()

	// graceful shutdown
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, os.Interrupt, syscall.SIGTERM)
	<-stopCh
	log.Println("shutting down server...")
	cancel()
	grpcServer.GracefulStop()
	log.Println("server stopped")
}
