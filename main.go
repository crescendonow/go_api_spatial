package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func main() {
	app := fiber.New()

	// ดึงค่า Environment Variables จาก Railway
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mongoURI := os.Getenv("MONGODB_URI")
	redisURL := os.Getenv("REDIS_URL")
	pythonURL := os.Getenv("PYTHON_SPATIAL_URL") // สำคัญมาก: ต้องตั้งค่าใน Railway

	// ---------------------------------------------------------
	// ส่วนเชื่อมต่อ Database (MongoDB & Redis)
	// ---------------------------------------------------------
	if mongoURI != "" {
		client, err := mongo.Connect(context.TODO(), options.Client().ApplyURI(mongoURI))
		if err == nil {
			log.Println("✅ Connected to MongoDB")
			defer client.Disconnect(context.TODO())
		}
	}

	if redisURL != "" {
		opt, _ := redis.ParseURL(redisURL)
		rdb := redis.NewClient(opt)
		if rdb.Ping(context.Background()).Err() == nil {
			log.Println("✅ Connected to Redis")
		}
	}

	// ---------------------------------------------------------
	// Endpoints
	// ---------------------------------------------------------

	// 1. Health Check
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"service":       "Go API Gateway",
			"status":        "OK",
			"python_target": pythonURL,
		})
	})

	// 2. Spatial Snap Endpoint (ยิงทะลุไปหา Python)
	app.Post("/api/spatial/snap", func(c *fiber.Ctx) error {
		// เช็คว่ามีการตั้งค่า PYTHON_SPATIAL_URL หรือยัง
		if pythonURL == "" {
			return c.Status(500).JSON(fiber.Map{
				"error": "PYTHON_SPATIAL_URL is not configured in environment variables",
			})
		}

		// ขั้นตอนที่ 1: ดึง Payload ข้อมูลดิบ (JSON) ที่ส่งมาจาก Postman/Frontend
		reqBody := c.Body()

		// ขั้นตอนที่ 2: สร้าง HTTP Request เตรียมยิงไปหา Python ใน Private Network
		targetURL := pythonURL + "/snap"
		req, err := http.NewRequest("POST", targetURL, bytes.NewBuffer(reqBody))
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to prepare request for Python"})
		}
		req.Header.Set("Content-Type", "application/json")

		// ขั้นตอนที่ 3: ยิง Request ออกไป (ตั้ง Timeout ไว้ 10 วินาที ป้องกันระบบค้าง)
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("❌ Error calling Python Spatial: %v", err)
			return c.Status(502).JSON(fiber.Map{
				"error": "Failed to connect to Python Spatial service. Check if it's running.",
				"details": err.Error(),
			})
		}
		defer resp.Body.Close() // สำคัญ: ป้องกัน Memory Leak

		// ขั้นตอนที่ 4: อ่านผลลัพธ์ที่ Python ประมวลผลเสร็จแล้วตอบกลับมา
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to read response from Python"})
		}

		// ขั้นตอนที่ 5: ส่งต่อผลลัพธ์นั้นกลับไปให้ Postman/Frontend ทันที
		c.Set("Content-Type", "application/json")
		return c.Status(resp.StatusCode).Send(respBody)
	})

	// เริ่มรัน Server
	log.Printf("🚀 Starting Go API on port %s", port)
	log.Fatal(app.Listen(":" + port))
}