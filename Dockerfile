FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wagering-api ./cmd/api

FROM alpine:3.21
RUN addgroup -S app && adduser -S app -G app
USER app
COPY --from=build /out/wagering-api /usr/local/bin/wagering-api
EXPOSE 8080
ENTRYPOINT ["wagering-api"]
