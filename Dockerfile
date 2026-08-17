# Build stage
FROM golang:1.24-alpine AS builder

# Set working directory for the build
WORKDIR /app

# Copy module files first to leverage Docker cache
COPY go.mod go.sum ./
RUN go mod download

# Copy all source files including client directory
COPY . .

# Build the Go app with static linking
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o main .

# Final stage
FROM alpine:3.18
RUN apk --no-cache add ca-certificates

# ARG APP_VERSION, will be set during build by github actions
ARG APP_VERSION=0.0.0
LABEL app.version=$APP_VERSION

# Set working directory in final image
WORKDIR /app

# Copy the compiled binary from builder
COPY --from=builder /app/main .

# Copy client directory from builder
COPY --from=builder /app/client ./client/

# Expose the port your app runs on
EXPOSE 3347

# Command to run the application
CMD ["/app/main"]
