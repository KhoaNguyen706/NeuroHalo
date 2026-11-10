# One Dockerfile for every Go program. Build context is the repo root.
#   docker build -f go.Dockerfile --build-arg SERVICE=gateway .
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY gen ./gen
COPY internal ./internal
ARG SERVICE
COPY ${SERVICE} ./${SERVICE}
RUN CGO_ENABLED=0 go build -o /out/app ./${SERVICE}

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
