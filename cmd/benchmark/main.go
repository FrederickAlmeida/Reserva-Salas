package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	bookingpb "github.com/FrederickAlmeida/Reserva-Salas/proto"
	"github.com/FrederickAlmeida/Reserva-Salas/internal/mq"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Metrics struct {
	Latencies    []time.Duration
	SuccessCount int64
	ErrorCount   int64
	TotalTime    time.Duration
}

type BenchmarkResult struct {
	Operation      string
	Middleware     string
	TotalRequests  int
	SuccessRate    float64
	AvgLatency     time.Duration
	P50Latency     time.Duration
	P95Latency     time.Duration
	P99Latency     time.Duration
	Throughput     float64
}

type GRPCClient struct {
	client bookingpb.ReservationServiceClient
	conn   *grpc.ClientConn
}

func NewGRPCClient(addr string) (*GRPCClient, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &GRPCClient{
		client: bookingpb.NewReservationServiceClient(conn),
		conn:   conn,
	}, nil
}

func (c *GRPCClient) Close() error {
	return c.conn.Close()
}

func (c *GRPCClient) ListAvailable(ctx context.Context, date string) error {
	_, err := c.client.ListAvailable(ctx, &bookingpb.ListAvailableRequest{Date: date})
	return err
}

func (c *GRPCClient) CreateReservation(ctx context.Context, roomID, date, start, end string) error {
	_, err := c.client.CreateReservation(ctx, &bookingpb.CreateReservationRequest{
		RoomId:    roomID,
		Date:      date,
		StartTime: start,
		EndTime:   end,
	})
	return err
}

func (c *GRPCClient) ConfirmReservation(ctx context.Context, reservationID string) error {
	_, err := c.client.ConfirmReservation(ctx, &bookingpb.ConfirmReservationRequest{
		ReservationId: reservationID,
	})
	return err
}

type MQClient struct {
	conn      *amqp.Connection
	ch        *amqp.Channel
	replies   <-chan amqp.Delivery
	replyTo   string
	queueName string
}

func NewMQClient(amqpURL, queueName string) (*MQClient, error) {
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		return nil, err
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, err
	}

	replyQueue, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}

	replies, err := ch.Consume(replyQueue.Name, "", true, true, false, false, nil)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}

	return &MQClient{
		conn:      conn,
		ch:        ch,
		replies:   replies,
		replyTo:   replyQueue.Name,
		queueName: queueName,
	}, nil
}

func (c *MQClient) Close() error {
	if c.ch != nil {
		c.ch.Close()
	}
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func (c *MQClient) sendCommandAndWait(ctx context.Context, env mq.CommandEnvelope) (mq.Response, error) {
	var zero mq.Response

	body, err := json.Marshal(env)
	if err != nil {
		return zero, err
	}

	correlationID := fmt.Sprintf("%d", time.Now().UnixNano())

	err = c.ch.PublishWithContext(ctx, "", c.queueName, false, false, amqp.Publishing{
		ContentType:   "application/json",
		ReplyTo:       c.replyTo,
		CorrelationId: correlationID,
		Body:          body,
	})
	if err != nil {
		return zero, err
	}

	for {
		select {
		case msg := <-c.replies:
			if msg.CorrelationId != correlationID {
				continue
			}
			var resp mq.Response
			if err := json.Unmarshal(msg.Body, &resp); err != nil {
				return zero, err
			}
			return resp, nil
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
}

func (c *MQClient) ListAvailable(ctx context.Context, date string) error {
	payload, _ := json.Marshal(mq.ListAvailablePayload{Date: date})
	env := mq.CommandEnvelope{
		Type:    mq.CommandListAvailable,
		Payload: payload,
	}
	resp, err := c.sendCommandAndWait(ctx, env)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf(resp.Error)
	}
	return nil
}

func (c *MQClient) CreateReservation(ctx context.Context, roomID, date, start, end string) error {
	payload, _ := json.Marshal(mq.CreateReservationPayload{
		RoomID:    roomID,
		Date:      date,
		StartTime: start,
		EndTime:   end,
	})
	env := mq.CommandEnvelope{
		Type:    mq.CommandCreateReservation,
		Payload: payload,
	}
	resp, err := c.sendCommandAndWait(ctx, env)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf(resp.Error)
	}
	return nil
}

func (c *MQClient) ConfirmReservation(ctx context.Context, reservationID string) error {
	payload, _ := json.Marshal(mq.ConfirmReservationPayload{ReservationID: reservationID})
	env := mq.CommandEnvelope{
		Type:    mq.CommandConfirmReservation,
		Payload: payload,
	}
	resp, err := c.sendCommandAndWait(ctx, env)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf(resp.Error)
	}
	return nil
}

type Client interface {
	ListAvailable(ctx context.Context, date string) error
	CreateReservation(ctx context.Context, roomID, date, start, end string) error
	ConfirmReservation(ctx context.Context, reservationID string) error
	Close() error
}

func calculatePercentile(latencies []time.Duration, percentile float64) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(float64(len(sorted)) * percentile / 100.0))
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func calculateMetrics(m Metrics) BenchmarkResult {
	if len(m.Latencies) == 0 {
		return BenchmarkResult{}
	}

	total := int64(len(m.Latencies))
	successRate := float64(m.SuccessCount) / float64(total) * 100.0

	var sum time.Duration
	for _, lat := range m.Latencies {
		sum += lat
	}
	avgLatency := sum / time.Duration(len(m.Latencies))

	throughput := float64(total) / m.TotalTime.Seconds()

	return BenchmarkResult{
		TotalRequests: int(total),
		SuccessRate:   successRate,
		AvgLatency:    avgLatency,
		P50Latency:    calculatePercentile(m.Latencies, 50),
		P95Latency:    calculatePercentile(m.Latencies, 95),
		P99Latency:    calculatePercentile(m.Latencies, 99),
		Throughput:    throughput,
	}
}

func warmup(ctx context.Context, client Client, operation string, iterations int) {
	date := "2025-12-31"
	roomID := "sala-110"
	start := "20:00"
	end := "21:00"

	for i := 0; i < iterations; i++ {
		switch operation {
		case "ListAvailable":
			_ = client.ListAvailable(ctx, date)
		case "CreateReservation":
			room := fmt.Sprintf("sala-%03d", 110-(i%10))
			hour := 20 - (i % 4)
			reqStart := fmt.Sprintf("%02d:00", hour)
			reqEnd := fmt.Sprintf("%02d:30", hour)
			_ = client.CreateReservation(ctx, room, date, reqStart, reqEnd)
		case "ConfirmReservation":
			resID := fmt.Sprintf("%s_%s_%s_%s", roomID, date, start, end)
			_ = client.ConfirmReservation(ctx, resID)
		}
	}
}

func runPerformanceTest(client Client, middleware, operation string, totalRequests, concurrency int) BenchmarkResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("[%s] Warmup para %s...", middleware, operation)
	warmupCtx, warmupCancel := context.WithTimeout(ctx, 5*time.Second)
	warmup(warmupCtx, client, operation, 50)
	warmupCancel()

	var metrics Metrics
	metrics.Latencies = make([]time.Duration, 0, totalRequests)
	var mu sync.Mutex

	requestCh := make(chan int, totalRequests)
	for i := 0; i < totalRequests; i++ {
		requestCh <- i
	}
	close(requestCh)

	startTime := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range requestCh {
				reqStart := time.Now()
				err := executeOperation(ctx, client, operation, i)
				latency := time.Since(reqStart)

				mu.Lock()
				metrics.Latencies = append(metrics.Latencies, latency)
				if err == nil {
					atomic.AddInt64(&metrics.SuccessCount, 1)
				} else {
					atomic.AddInt64(&metrics.ErrorCount, 1)
				}
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	metrics.TotalTime = time.Since(startTime)

	result := calculateMetrics(metrics)
	result.Operation = operation
	result.Middleware = middleware
	return result
}

func executeOperation(ctx context.Context, client Client, operation string, workerID int) error {
	date := "2025-12-01"
	roomID := fmt.Sprintf("sala-%03d", 101+(workerID%10))
	start := "09:00"
	end := "10:00"

	switch operation {
	case "ListAvailable":
		return client.ListAvailable(ctx, date)
	case "CreateReservation":
		hour := 9 + (workerID % 8)
		start = fmt.Sprintf("%02d:00", hour)
		end = fmt.Sprintf("%02d:30", hour)
		return client.CreateReservation(ctx, roomID, date, start, end)
	case "ConfirmReservation":
		resID := fmt.Sprintf("%s_%s_%s_%s", roomID, date, start, end)
		return client.ConfirmReservation(ctx, resID)
	default:
		return fmt.Errorf("unknown operation: %s", operation)
	}
}

type BusinessFactorResult struct {
	Middleware    string
	Scenario      string
	TotalRequests int
	SuccessCount  int
	SuccessRate   float64
	ExpectedMax   int
}

func runBusinessFactorTest(client Client, middleware string, sameResource bool) BusinessFactorResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("[%s] Warmup para Fator de Negócio...", middleware)
	warmupCtx, warmupCancel := context.WithTimeout(ctx, 2*time.Second)
	for i := 0; i < 10; i++ {
		_ = client.ListAvailable(warmupCtx, "2025-12-01")
	}
	warmupCancel()

	totalRequests := 200
	concurrency := 20

	var successCount int64

	requestCh := make(chan int, totalRequests)
	for i := 0; i < totalRequests; i++ {
		requestCh <- i
	}
	close(requestCh)

	date := "2025-12-02"
	if sameResource {
		date = "2025-12-02"
	} else {
		date = "2025-12-01"
	}
	roomID := "sala-101"
	start := "09:00"
	end := "10:00"

	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for reqID := range requestCh {
				var err error
				if sameResource {
					err = client.CreateReservation(ctx, roomID, date, start, end)
				} else {
					roomIdx := reqID % 10
					hourIdx := reqID / 10
					room := fmt.Sprintf("sala-%03d", 101+roomIdx)
					startHour := 9 + (hourIdx / 2)
					startMinute := (hourIdx % 2) * 30
					reqStart := fmt.Sprintf("%02d:%02d", startHour, startMinute)
					endMinute := startMinute + 30
					endHour := startHour
					if endMinute >= 60 {
						endMinute = 0
						endHour++
					}
					reqEnd := fmt.Sprintf("%02d:%02d", endHour, endMinute)
					err = client.CreateReservation(ctx, room, date, reqStart, reqEnd)
				}

				if err == nil {
					atomic.AddInt64(&successCount, 1)
				}
			}
		}(i)
	}

	wg.Wait()

	success := int(atomic.LoadInt64(&successCount))
	successRate := float64(success) / float64(totalRequests) * 100.0

	scenario := "Recursos Diferentes"
	expectedMax := totalRequests
	if sameResource {
		scenario = "Mesmo Recurso"
		expectedMax = 1
	}

	return BusinessFactorResult{
		Middleware:    middleware,
		Scenario:      scenario,
		TotalRequests: totalRequests,
		SuccessCount:  success,
		SuccessRate:   successRate,
		ExpectedMax:   expectedMax,
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Uso: benchmark [performance|business]")
		os.Exit(1)
	}

	testType := os.Args[1]

	grpcAddr := os.Getenv("BOOKING_ADDR")
	if grpcAddr == "" {
		grpcAddr = "localhost:50051"
	}

	amqpURL := os.Getenv("AMQP_URL")
	if amqpURL == "" {
		amqpURL = "amqp://guest:guest@localhost:5672/"
	}

	queueName := os.Getenv("BOOKING_QUEUE")
	if queueName == "" {
		queueName = "booking.commands"
	}

	switch testType {
	case "performance":
		runPerformanceBenchmark(grpcAddr, amqpURL, queueName)
	case "business":
		runBusinessFactorBenchmark(grpcAddr, amqpURL, queueName)
	default:
		fmt.Printf("Tipo de teste desconhecido: %s\n", testType)
		os.Exit(1)
	}
}

func runPerformanceBenchmark(grpcAddr, amqpURL, queueName string) {
	fmt.Println("=" + strings.Repeat("=", 80))
	fmt.Println("TESTE DE PERFORMANCE GERAL")
	fmt.Println("=" + strings.Repeat("=", 80))

	operations := []string{"ListAvailable", "CreateReservation", "ConfirmReservation"}
	totalRequests := 1000
	concurrency := 10

	var results []BenchmarkResult

	fmt.Println("\n--- Testando gRPC ---")
	grpcClient, err := NewGRPCClient(grpcAddr)
	if err != nil {
		log.Fatalf("Erro ao conectar gRPC: %v", err)
	}
	defer grpcClient.Close()

	for _, op := range operations {
		fmt.Printf("\nExecutando %s...\n", op)
		result := runPerformanceTest(grpcClient, "gRPC", op, totalRequests, concurrency)
		results = append(results, result)
		printResult(result)
	}

	fmt.Println("\n--- Testando RabbitMQ ---")
	mqClient, err := NewMQClient(amqpURL, queueName)
	if err != nil {
		log.Fatalf("Erro ao conectar RabbitMQ: %v", err)
	}
	defer mqClient.Close()

	for _, op := range operations {
		fmt.Printf("\nExecutando %s...\n", op)
		result := runPerformanceTest(mqClient, "RabbitMQ", op, totalRequests, concurrency)
		results = append(results, result)
		printResult(result)
	}

	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("RESUMO COMPARATIVO")
	fmt.Println(strings.Repeat("=", 80))
	printComparison(results)
}

func runBusinessFactorBenchmark(grpcAddr, amqpURL, queueName string) {
	fmt.Println("=" + strings.Repeat("=", 80))
	fmt.Println("TESTE DE FATOR DE NEGÓCIO")
	fmt.Println("=" + strings.Repeat("=", 80))

	var results []BusinessFactorResult

	fmt.Println("\n--- Testando gRPC ---")
	grpcClient, err := NewGRPCClient(grpcAddr)
	if err != nil {
		log.Fatalf("Erro ao conectar gRPC: %v", err)
	}
	defer grpcClient.Close()

	fmt.Println("\nCenário: Recursos Diferentes")
	result1 := runBusinessFactorTest(grpcClient, "gRPC", false)
	results = append(results, result1)
	printBusinessResult(result1)

	fmt.Println("\nCenário: Mesmo Recurso")
	result2 := runBusinessFactorTest(grpcClient, "gRPC", true)
	results = append(results, result2)
	printBusinessResult(result2)

	fmt.Println("\n--- Testando RabbitMQ ---")
	mqClient, err := NewMQClient(amqpURL, queueName)
	if err != nil {
		log.Fatalf("Erro ao conectar RabbitMQ: %v", err)
	}
	defer mqClient.Close()

	fmt.Println("\nCenário: Recursos Diferentes")
	result3 := runBusinessFactorTest(mqClient, "RabbitMQ", false)
	results = append(results, result3)
	printBusinessResult(result3)

	fmt.Println("\nCenário: Mesmo Recurso")
	result4 := runBusinessFactorTest(mqClient, "RabbitMQ", true)
	results = append(results, result4)
	printBusinessResult(result4)
}

func printResult(r BenchmarkResult) {
	fmt.Printf("\n%s - %s:\n", r.Middleware, r.Operation)
	fmt.Printf("  Total de Requisições: %d\n", r.TotalRequests)
	fmt.Printf("  Taxa de Sucesso: %.2f%%\n", r.SuccessRate)
	fmt.Printf("  Latência Média: %v\n", r.AvgLatency)
	fmt.Printf("  P50: %v\n", r.P50Latency)
	fmt.Printf("  P95: %v\n", r.P95Latency)
	fmt.Printf("  P99: %v\n", r.P99Latency)
	fmt.Printf("  Throughput: %.2f req/s\n", r.Throughput)
}

func printBusinessResult(r BusinessFactorResult) {
	fmt.Printf("\n[%s] %s:\n", r.Middleware, r.Scenario)
	fmt.Printf("  Total de Requisições: %d\n", r.TotalRequests)
	fmt.Printf("  Sucessos: %d\n", r.SuccessCount)
	fmt.Printf("  Taxa de Sucesso: %.2f%%\n", r.SuccessRate)
	fmt.Printf("  Máximo Esperado: %d\n", r.ExpectedMax)
	if r.Scenario == "Mesmo Recurso" && r.SuccessCount > r.ExpectedMax {
		fmt.Printf("  ⚠️  AVISO: Mais de %d reserva aceita! Possível race condition.\n", r.ExpectedMax)
	}
}

func printComparison(results []BenchmarkResult) {
	ops := map[string][]BenchmarkResult{}
	for _, r := range results {
		ops[r.Operation] = append(ops[r.Operation], r)
	}

	for op, res := range ops {
		if len(res) < 2 {
			continue
		}
		fmt.Printf("\n%s:\n", op)
		for _, r := range res {
			fmt.Printf("  %s: Latência Média=%v, Throughput=%.2f req/s, Sucesso=%.2f%%\n",
				r.Middleware, r.AvgLatency, r.Throughput, r.SuccessRate)
		}
	}
}
