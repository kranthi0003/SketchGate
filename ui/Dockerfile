# Go Dockerfile for SketchGate UI
FROM golang:1.21-alpine
WORKDIR /app
COPY . .
WORKDIR /app/ui
RUN go mod init ui || true
RUN go mod tidy
EXPOSE 8080
CMD ["go", "run", "main.go"]