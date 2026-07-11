# Pure-Go build (modernc sqlite + all deps are cgo-free) → static binary on a
# distroless base. No external services: xilo is the whole cache.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
# Admin CSS is a generated artifact (not committed); build it before compiling
# since it's embedded via go:embed. Tailwind standalone CLI, musl build.
RUN apk add --no-cache bash \
 && wget -qO /usr/local/bin/tailwindcss https://github.com/tailwindlabs/tailwindcss/releases/latest/download/tailwindcss-linux-x64-musl \
 && chmod +x /usr/local/bin/tailwindcss \
 && sh scripts/build-css.sh
# Views are generated at build time (*_templ.go is not committed); templ's
# version comes from go.mod so it can't drift.
RUN go run github.com/a-h/templ/cmd/templ@$(go list -m -f '{{.Version}}' github.com/a-h/templ) generate
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /xilo ./cmd/xilo

FROM gcr.io/distroless/static-debian12
COPY --from=build /xilo /xilo
# Defaults: config at /xilo.yaml (mount to override), data + local storage at
# /data (declare as a volume), listen on 8080.
# GOMEMLIMIT keeps steady-state RSS predictable on small VPSes (Go's GC
# otherwise lets the heap balloon under parallel NAR streaming). Override
# with -e GOMEMLIMIT=1GiB on bigger machines if you want more page cache
# headroom traded for fewer GC cycles.
ENV XILO_LISTEN=":8080" GOMEMLIMIT="192MiB"
WORKDIR /
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/xilo"]
CMD ["serve"]
