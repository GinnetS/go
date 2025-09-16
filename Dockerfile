# Build stage
FROM golang:1.22-alpine AS builder
WORKDIR /app

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64 \
    GOPROXY=https://proxy.golang.org,direct

# Copiamos solo lo necesario para resolver deps primero (mejor cache)
COPY go.mod ./
# Si ya tienes go.sum, cópialo (no es obligatorio, pero acelera)
# COPY go.sum ./

# Para que 'go mod tidy' pueda resolver imports, copiamos main.go
COPY main.go ./

# Genera/actualiza go.sum dentro del contenedor
RUN go mod tidy

# (opcional) si tienes más fuentes, súbelas ahora
# COPY . .

# Compila
RUN mkdir -p /out && go build -trimpath -ldflags "-s -w" -o /out/pingpool main.go

# Runtime stage
FROM alpine:3.20
WORKDIR /app
COPY --from=builder /out/pingpool /usr/local/bin/pingpool
COPY hosts.txt ./hosts.txt
ENTRYPOINT ["/usr/local/bin/pingpool"]
