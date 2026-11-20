# Builds the Logbook server as a static binary on a minimal base image.
# docker build -f deploy/Dockerfile -t logbook-server .
FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/logbook-server ./cmd/logbook-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/logbook-server /logbook-server
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/logbook-server"]
