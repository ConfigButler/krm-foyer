# The krm-foyer image. `task image` builds it; CI and the release workflow run
# the same task, so there is one definition of what ships.

# Pinned by digest; Dependabot's docker ecosystem moves version and digest together.
FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build

WORKDIR /src
COPY go.mod ./
# go.sum does not exist until the first dependency does; the glob keeps this
# layer valid either way.
COPY go.su[m] ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG VERSION=dev
# CGO off: the runtime image has no libc. -trimpath keeps build paths out of the binary.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/krm-foyer ./cmd/krm-foyer

# distroless static, nonroot: no shell, no package manager, UID 65532.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=build /out/krm-foyer /krm-foyer
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/krm-foyer"]
