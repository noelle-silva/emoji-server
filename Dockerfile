FROM golang:1.22-alpine AS build

WORKDIR /src

COPY . .

RUN go build -trimpath -ldflags="-s -w" -o /out/emoji-server ./cmd/emoji-server

FROM alpine:3.20

RUN addgroup -S app && adduser -S -G app app

WORKDIR /app
COPY --from=build /out/emoji-server /app/emoji-server

USER app:app
ENTRYPOINT ["/app/emoji-server"]
