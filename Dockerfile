FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/forge ./cmd/forge

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/forge /forge
VOLUME ["/data"]
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/forge"]
CMD ["serve", "--listen", ":8080", "--db", "/data/forge.db"]
