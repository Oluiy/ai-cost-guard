FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /ai-guard ./cmd/ai-guard

FROM alpine:3.20
RUN adduser -D -h /data aiguard
COPY --from=build /ai-guard /usr/local/bin/ai-guard
USER aiguard
WORKDIR /data
EXPOSE 8787
ENTRYPOINT ["ai-guard"]
CMD ["run", "--config", "/data/config.yaml"]
