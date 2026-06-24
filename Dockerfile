# Builds the burrowd control-plane image (FSL-1.1-ALv2 — see LICENSING.md). This is the
# only Burrow binary that runs in-cluster; the CLI and MCP server run on the developer's
# machine. `burrow install` deploys this image.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/burrowd ./cmd/burrowd

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/burrowd /burrowd
USER nonroot:nonroot
ENTRYPOINT ["/burrowd"]
