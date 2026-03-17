FROM golang:1.25.6-alpine AS build
WORKDIR /app
COPY go.mod .
COPY go.sum .
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o pinless

FROM scratch
COPY --from=build /app/pinless /usr/local/bin/pinless
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
EXPOSE 3000
CMD ["pinless"]
