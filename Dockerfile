# Reference multi-stage build for an agentscope-go service.
#
# agentscope-go is a library; this builds one of the runnable examples so the
# image is a concrete, deployable reference. Override the target with:
#   docker build --build-arg EXAMPLE=agent_service -t agentscope-go .
#
# The final image is distroless and runs as a non-root user.

FROM golang:1.25@sha256:699337d620559a59b4a2bb298ad59611e535d2ee755a34cf2d2a98f37578dc80 AS build
WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG EXAMPLE=agent_service
# Static, stripped binary.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/app ./examples/${EXAMPLE}

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
WORKDIR /
COPY --from=build /out/app /app
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app"]
