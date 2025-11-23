package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	bookingpb "github.com/FrederickAlmeida/Reserva-Salas/proto"
	"google.golang.org/grpc"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: client [list|reserve|cancel|confirm] [flags]")
		os.Exit(1)
	}

	addr := os.Getenv("BOOKING_ADDR")
	if addr == "" {
		addr = "localhost:50051"
	}

	conn, err := grpc.Dial(addr, grpc.WithInsecure())
	if err != nil {
		log.Fatalf("failed to connect to server: %v", err)
	}
	defer conn.Close()

	client := bookingpb.NewReservationServiceClient(conn)

	cmd := os.Args[1]
	switch cmd {
	case "list":
		listCmd(client, os.Args[2:])
	case "reserve":
		reserveCmd(client, os.Args[2:])
	case "cancel":
		cancelCmd(client, os.Args[2:])
	case "confirm":
		confirmCmd(client, os.Args[2:])
	default:
		fmt.Println("unknown command:", cmd)
		os.Exit(1)
	}
}

func listCmd(client bookingpb.ReservationServiceClient, args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	date := fs.String("date", "", "date (YYYY-MM-DD)")
	_ = fs.Parse(args)

	if *date == "" {
		log.Fatal("date is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.ListAvailable(ctx, &bookingpb.ListAvailableRequest{Date: *date})
	if err != nil {
		log.Fatalf("ListAvailable error: %v", err)
	}

	fmt.Printf("Salas livres em %s:\n", *date)
	for _, room := range resp.GetRooms() {
		fmt.Printf("- %s:\n", room.GetRoomId())
		if len(room.GetFreeSlots()) == 0 {
			fmt.Println("  (sem horários livres)")
			continue
		}
		for _, slot := range room.GetFreeSlots() {
			fmt.Printf("  %s - %s\n", slot.GetStartTime(), slot.GetEndTime())
		}
	}
}

func reserveCmd(client bookingpb.ReservationServiceClient, args []string) {
	fs := flag.NewFlagSet("reserve", flag.ExitOnError)
	room := fs.String("room", "", "room id (ex: sala-101)")
	date := fs.String("date", "", "date (YYYY-MM-DD)")
	start := fs.String("start", "", "start time (HH:MM)")
	end := fs.String("end", "", "end time (HH:MM)")
	_ = fs.Parse(args)

	if *room == "" || *date == "" || *start == "" || *end == "" {
		log.Fatal("room, date, start and end are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.CreateReservation(ctx, &bookingpb.CreateReservationRequest{
		RoomId:    *room,
		Date:      *date,
		StartTime: *start,
		EndTime:   *end,
	})
	if err != nil {
		log.Fatalf("CreateReservation error: %v", err)
	}

	fmt.Printf("Reserva criada. ID: %s | Status: %s | Msg: %s\n",
		resp.GetReservationId(), resp.GetStatus().String(), resp.GetMessage())
}

func cancelCmd(client bookingpb.ReservationServiceClient, args []string) {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	id := fs.String("id", "", "reservation id")
	_ = fs.Parse(args)

	if *id == "" {
		log.Fatal("id is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.CancelReservation(ctx, &bookingpb.CancelReservationRequest{
		ReservationId: *id,
	})
	if err != nil {
		log.Fatalf("CancelReservation error: %v", err)
	}

	fmt.Printf("Cancelamento: Status: %s | Msg: %s\n",
		resp.GetStatus().String(), resp.GetMessage())
}

func confirmCmd(client bookingpb.ReservationServiceClient, args []string) {
	fs := flag.NewFlagSet("confirm", flag.ExitOnError)
	id := fs.String("id", "", "reservation id")
	_ = fs.Parse(args)

	if *id == "" {
		log.Fatal("id is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.ConfirmReservation(ctx, &bookingpb.ConfirmReservationRequest{
		ReservationId: *id,
	})
	if err != nil {
		log.Fatalf("ConfirmReservation error: %v", err)
	}

	fmt.Printf("Confirmação: Status: %s | Msg: %s\n",
		resp.GetStatus().String(), resp.GetMessage())
}
