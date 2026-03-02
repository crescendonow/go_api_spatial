# Build Stage
FROM golang:1.21-alpine AS builder
WORKDIR /app

# ก๊อปปี้โค้ดทั้งหมดเข้ามาใน Docker ก่อน
COPY . .

# ให้ Docker สั่งดึงไลบรารีใหม่ๆ ที่มีใน main.go ให้อัตโนมัติ (ข้าม error go.sum ไปได้เลย)
RUN go mod tidy

# เริ่ม Build
RUN go build -o main .

# Run Stage
FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/main .
EXPOSE 8080
CMD ["./main"]