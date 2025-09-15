FROM golang:1.23-bookworm AS build
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/proxy ./main.go

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --chown=nonroot:nonroot --chmod=0755 /out/proxy /proxy
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/proxy"]