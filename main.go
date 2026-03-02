package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func main() {
	app := fiber.New()
	ctx := context.Background()

	// 1. ตัวแปร Environment Variables
	port := os.Getenv("PORT")
	if port == "" { port = "8080" }
	mongoURI := os.Getenv("MONGODB_URI")
	redisURL := os.Getenv("REDIS_URL")
	dbURL := os.Getenv("DATABASE_URL")
	pythonURL := os.Getenv("PYTHON_SPATIAL_URL")

	// ---------------------------------------------------------
	// 2. เชื่อมต่อฐานข้อมูลทั้ง 3 ตัว
	// ---------------------------------------------------------
	
	// Redis (Caching Layer)
	var rdb *redis.Client
	if redisURL != "" {
		opt, _ := redis.ParseURL(redisURL)
		rdb = redis.NewClient(opt)
		if err := rdb.Ping(ctx).Err(); err == nil {
			log.Println("✅ Connected to Redis")
		}
	}

	// MongoDB (Customer Data)
	var mongoDb *mongo.Database
	if mongoURI != "" {
		client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
		if err == nil {
			log.Println("✅ Connected to MongoDB")
			mongoDb = client.Database("spatial_edit") // ตั้งชื่อ Database
		}
	}

	// PostGIS (Geometry Storage)
	var pgPool *pgxpool.Pool
	if dbURL != "" {
		pool, err := pgxpool.New(ctx, dbURL)
		if err == nil {
			pgPool = pool
			log.Println("✅ Connected to PostGIS")
			defer pgPool.Close()
		} else {
			// พ่น Error ออกมาถ้าต่อไม่ติด
			log.Printf("❌ Failed to connect to PostGIS: %v\n", err) 
		}
	} else {
		// แจ้งเตือนถ้าลืมใส่ตัวแปร
		log.Println("⚠️ DATABASE_URL is missing. Skipping PostGIS connection.") 
	}

	// ---------------------------------------------------------
	// 3. Endpoints การทำงานจริง
	// ---------------------------------------------------------

	// Flow 3.3: อ่านข้อมูลลูกค้า (MongoDB + Redis Cache)
	app.Get("/api/customers/:id", func(c *fiber.Ctx) error {
		id := c.Params("id")
		cacheKey := "customer:" + id // รูปแบบ Key ตาม Architecture

		// ขั้นที่ 1: เช็ค Redis (Cache HIT)
		if rdb != nil {
			cachedData, err := rdb.Get(ctx, cacheKey).Result()
			if err == redis.Nil {
				// Cache MISS (ไปต่อที่ MongoDB)
			} else if err == nil {
				c.Set("Content-Type", "application/json")
				c.Set("X-Cache", "HIT")
				return c.SendString(cachedData)
			}
		}

		// ขั้นที่ 2: ดึงจาก MongoDB
		if mongoDb == nil {
			return c.Status(500).JSON(fiber.Map{"error": "MongoDB not connected"})
		}
		var customer bson.M
		coll := mongoDb.Collection("customers")
		err := coll.FindOne(ctx, bson.M{"_id": id}).Decode(&customer)
		if err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "Customer not found"})
		}

		// ขั้นที่ 3: บันทึกลง Redis พร้อมตั้ง TTL 15 นาที
		custJSON, _ := json.Marshal(customer)
		if rdb != nil {
			rdb.Set(ctx, cacheKey, custJSON, 15*time.Minute) 
		}

		c.Set("X-Cache", "MISS")
		return c.JSON(customer)
	})

	// Flow 3.1: ประมวลผลเชิงพื้นที่ (Python + PostGIS + Redis)
	app.Post("/api/spatial/snap", func(c *fiber.Ctx) error {
		reqBody := c.Body()

		// สร้าง Hash จาก Payload เพื่อทำ Cache Key: spatial:snap:{hash}
		hash := sha256.Sum256(reqBody)
		cacheKey := "spatial:snap:" + hex.EncodeToString(hash[:16]) 

		// ขั้นที่ 1: เช็ค Redis ก่อนคำนวณซ้ำ
		if rdb != nil {
			cachedSnap, err := rdb.Get(ctx, cacheKey).Result()
			if err == nil {
				c.Set("Content-Type", "application/json")
				c.Set("X-Cache", "HIT")
				return c.SendString(cachedSnap) // ส่งผลลัพธ์เก่ากลับทันที ลดภาระ Python
			}
		}

		// ขั้นที่ 2: โยนให้ Python จัดการ
		resp, err := http.Post(pythonURL+"/snap", "application/json", bytes.NewBuffer(reqBody))
		if err != nil || resp.StatusCode != 200 {
			return c.Status(502).JSON(fiber.Map{"error": "Failed to call Python service"})
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)

		// ขั้นที่ 3: บันทึกข้อมูลพิกัดลง PostGIS (ตัวอย่างการแปลง GeoJSON เป็น Geometry)
		// **หมายเหตุ:** ต้องมีการสร้าง Table 'geometries' ไว้ก่อนใน DB 
		if pgPool != nil {
			// สมมติว่า Python คืนค่า {"data": {"type": "Point", "coordinates": [...]}} กลับมา
			query := `INSERT INTO geometries (geom) VALUES (ST_SetSRID(ST_GeomFromGeoJSON($1), 4326))`
			_, pgErr := pgPool.Exec(ctx, query, string(reqBody))
			if pgErr != nil {
				log.Printf("⚠️ PostGIS Insert Warning: %v", pgErr)
				// ปล่อยผ่านไปก่อน เผื่อ Table ยังไม่สร้าง
			}
		}

		// ขั้นที่ 4: บันทึกผลลัพธ์ลง Redis (TTL 30 นาที)
		if rdb != nil {
			rdb.Set(ctx, cacheKey, respBody, 30*time.Minute)
		}

		c.Set("Content-Type", "application/json")
		c.Set("X-Cache", "MISS")
		return c.Send(respBody)
	})

	log.Printf("🚀 Starting Go API on port %s", port)
	log.Fatal(app.Listen(":" + port))
}