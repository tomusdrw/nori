# Build stage
FROM golang:1.25-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go run github.com/a-h/templ/cmd/templ@$(go list -m -f '{{.Version}}' github.com/a-h/templ) generate
RUN CGO_ENABLED=0 go build -o /nori ./cmd/nori

# Runtime stage
FROM alpine:3.21
RUN apk add --no-cache bash ca-certificates docker-cli tmux
COPY --from=build /nori /usr/local/bin/nori
EXPOSE 8080
VOLUME ["/data", "/config"]
WORKDIR /data
ENTRYPOINT ["nori"]
