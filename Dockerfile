# Reference multi-stage build for an agentscope-go service.
#
# agentscope-go is a library; this builds one of the runnable examples so the
# image is a concrete, deployable reference. Override the target with:
#   docker build --build-arg EXAMPLE=agent_service -t agentscope-go .
#
# The final image is distroless and runs as a non-root user.

FROM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
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
