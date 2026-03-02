package main

import (
	"context"
	"log"
	"os"

	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func main() {
	app := fiber.New()

	// 1. รับค่า Environment Variables จาก Railway
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mongoURI := os.Getenv("MONGODB_URI")
	redisURL := os.Getenv("REDIS_URL")
	pythonURL := os.Getenv("PYTHON_SPATIAL_URL")

	// 2. จำลองการเชื่อมต่อ MongoDB
	if mongoURI != "" {
		client, err := mongo.Connect(context.TODO(), options.Client().ApplyURI(mongoURI))
		if err == nil {
			log.Println("✅ Connected to MongoDB")
			defer client.Disconnect(context.TODO())
		}
	}

	// 3. จำลองการเชื่อมต่อ Redis
	if redisURL != "" {
		opt, _ := redis.ParseURL(redisURL)
		rdb := redis.NewClient(opt)
		if rdb.Ping(context.Background()).Err() == nil {
			log.Println("✅ Connected to Redis")
		}
	}

	// 4. สร้าง Endpoint ทดสอบ
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"service": "Go API Gateway",
			"status":  "OK",
			"python_target": pythonURL,
		})
	})

	log.Printf("🚀 Starting Go API on port %s", port)
	log.Fatal(app.Listen(":" + port))
}