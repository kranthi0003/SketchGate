# Go Dockerfile for SketchGate UI
FROM golang:1.21-alpine
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY ui/ ./ui/
WORKDIR /app/ui
EXPOSE 8080
CMD ["go", "run", "main.go"]