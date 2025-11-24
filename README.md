# Reserva-Salas
Este projeto implementa um sistema distribuído de **reserva de recursos** (salas) em Go.


## Implementação com gRPC
Rodando:

### Pré-requisitos
- Go >= 1.20
- `protoc` e plugins (`protoc-gen-go`, `protoc-gen-go-grpc`) – apenas se quiser regenerar os arquivos `.pb.go`.
    

### Gerar código protobuf (opcional, já está gerado)
`protoc \   --go_out=. --go_opt=paths=source_relative \   --go-grpc_out=. --go-grpc_opt=paths=source_relative \   proto/reservation.proto`

### Rodando o servidor
`go run ./cmd/server`

Por padrão, o servidor escuta em `:50051`.  
Para mudar:
`BOOKING_ADDR=":6000" go run ./cmd/server`

### Rodando o cliente
Exemplos:

```bash
# Listar salas livres em um dia 
go run ./cmd/client list --date 2025-11-24  
# Criar uma reserva 
go run ./cmd/client reserve \   --room sala-101 \   --date 2025-11-24 \   --start 09:00 \   --end 10:00  
# Confirmar a reserva 
go run ./cmd/client confirm --id sala-101_2025-11-24_09:00_10:00  
# Cancelar a reserva 
go run ./cmd/client cancel --id sala-101_2025-11-24_09:00_10:00
```

## Implementação com rabbitMQ
Rodando:
1. Subir o rabbitMQ (via Docker)
```bash
docker run -d --name rabbitmq -p 5672:5672 -p 15672:15672 rabbitmq:3-management
```

2. Rodar o worker
`go run ./cmd/mq-worker`
Variáveis opcionais:
- AMQP_URL (default: amqp://guest:guest@localhost:5672/)
- BOOKING_QUEUE (default: booking.commands)

3. Rodar o cliente
Em outro terminal:
```bash
# Listar salas livres
go run ./cmd/mq-client list --date 2025-11-24

# Criar reserva
go run ./cmd/mq-client reserve \
  --room sala-101 \
  --date 2025-11-24 \
  --start 09:00 \
  --end 10:00

# Confirmar reserva
go run ./cmd/mq-client confirm --id sala-101_2025-11-24_09:00_10:00

# Cancelar reserva
go run ./cmd/mq-client cancel --id sala-101_2025-11-24_09:00_10:00
```