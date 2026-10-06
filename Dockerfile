FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/berth ./cmd/berth

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/berth /berth
VOLUME ["/data"]
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/berth"]
CMD ["serve", "--listen", ":8080", "--db", "/data/berth.db"]
