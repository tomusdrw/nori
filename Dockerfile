# Build stage
FROM golang:1.23-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go run github.com/a-h/templ/cmd/templ@v0.3.1020 generate
RUN CGO_ENABLED=0 go build -o /deploybot ./cmd/deploybot

# Runtime stage
FROM alpine:3.21
RUN apk add --no-cache bash ca-certificates docker-cli
COPY --from=build /deploybot /usr/local/bin/deploybot
EXPOSE 8080
VOLUME ["/data"]
ENV DEPLOYBOT_DB=/data/deploybot.db
ENTRYPOINT ["deploybot"]
