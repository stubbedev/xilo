# Pure-Go build (modernc sqlite + all deps are cgo-free) → static binary on a
# distroless base. No external services: xilo is the whole cache.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
# Views are generated at build time (*_templ.go is not committed); templ's
# version comes from go.mod so it can't drift.
RUN go run github.com/a-h/templ/cmd/templ@$(go list -m -f '{{.Version}}' github.com/a-h/templ) generate
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /xilo ./cmd/xilo

FROM gcr.io/distroless/static-debian12
COPY --from=build /xilo /xilo
# Defaults: config at /xilo.yaml (mount to override), data + local storage at
# /data (declare as a volume), listen on 8080.
ENV XILO_LISTEN=":8080"
WORKDIR /
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/xilo"]
CMD ["serve"]
