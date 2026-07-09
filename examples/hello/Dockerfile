# Build the hello server into a tiny distroless image — the same static-binary-on-distroless
# shape Burrow's own burrowd image uses (.ko.yaml). The build context is this directory only;
# hello has no dependencies beyond the standard library, so the build needs no network.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -o /hello .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /hello /hello
EXPOSE 8080
ENTRYPOINT ["/hello"]
