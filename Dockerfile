# Reference multi-stage build for an agentscope-go service.
#
# agentscope-go is a library; this builds one of the runnable examples so the
# image is a concrete, deployable reference. Override the target with:
#   docker build --build-arg EXAMPLE=agent_service -t agentscope-go .
#
# The final image is distroless and runs as a non-root user.

FROM golang:1.25 AS build
WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG EXAMPLE=agent_service
# Static, stripped binary.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/app ./examples/${EXAMPLE}

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/app /app
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app"]
