# --- Build Stage ---
ARG GO_VERSION=1.24
FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /workspace

# Copy Go modules and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY . .

# Build the application
# CGO_ENABLED=0 produces a static binary
# -ldflags="-w -s" reduces the size of the binary by removing debug information
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o pod-housekeeper main.go pod-housekeeper.go

# --- Final Stage ---
# Use a minimal non-root image
FROM gcr.io/distroless/static-debian12 AS final

# Create a non-root user (UID 65532 is standard for 'nobody' in distroless)
USER 65532:65532

WORKDIR /

# Copy the static binary from the builder stage
COPY --from=builder /workspace/pod-housekeeper .

# Run the binary
ENTRYPOINT ["/pod-housekeeper"] 