FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /fitguard ./cmd/fitguard

FROM scratch
# scratch has no trust store of its own. fitguard's entire job is calling
# OpenAI/Anthropic/Gemini/Groq/Together over HTTPS, so without this every
# upstream request fails TLS verification (x509: certificate signed by
# unknown authority) rather than the image just being minimal.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /fitguard /fitguard
EXPOSE 8787
ENTRYPOINT ["/fitguard"]
CMD ["run", "--config", "/data/config.yaml"]
