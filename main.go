package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
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

	port := os.Getenv("PORT")
	if port == "" { port = "8080" }
	mongoURI := os.Getenv("MONGODB_URI")
	redisURL := os.Getenv("REDIS_URL")
	dbURL := os.Getenv("DATABASE_URL")
	pythonURL := os.Getenv("PYTHON_SPATIAL_URL")

	// --- Database Connections ---
	var rdb *redis.Client
	if redisURL != "" {
		opt, _ := redis.ParseURL(redisURL)
		rdb = redis.NewClient(opt)
		if err := rdb.Ping(ctx).Err(); err == nil {
			log.Println("✅ Connected to Redis")
		}
	}

	var mongoDb *mongo.Database
	if mongoURI != "" {
		client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
		if err == nil {
			log.Println("✅ Connected to MongoDB")
			mongoDb = client.Database("spatial_edit")
		}
	}

	var pgPool *pgxpool.Pool
	if dbURL != "" {
		pool, err := pgxpool.New(ctx, dbURL)
		if err == nil {
			pgPool = pool
			log.Println("✅ Connected to PostGIS")
			defer pgPool.Close()
		} else {
			log.Printf("❌ Failed to connect to PostGIS: %v\n", err)
		}
	} else {
		log.Println("⚠️ DATABASE_URL is missing. Skipping PostGIS connection.")
	}

	// --- Endpoints ---

	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "OK"})
	})

	// 📍 Flow 3.2: Customer Proximity Search (MongoDB $nearSphere + Redis Cache)
	app.Get("/api/customers", func(c *fiber.Ctx) error {
		// 1. รับค่าจาก Query Parameter
		lngStr := c.Query("lng")
		latStr := c.Query("lat")
		radiusStr := c.Query("radius", "5000") // ค่าเริ่มต้น 5000 เมตร (5km)

		// แปลงค่าเป็นตัวเลข (float64)
		lng, errLng := strconv.ParseFloat(lngStr, 64)
		lat, errLat := strconv.ParseFloat(latStr, 64)
		radius, errRad := strconv.ParseFloat(radiusStr, 64)

		if errLng != nil || errLat != nil || errRad != nil {
			return c.Status(400).JSON(fiber.Map{"error": "Invalid parameters. Require lng, lat, and radius as numbers."})
		}

		// 2. สร้าง Cache Key (ใช้ทศนิยม 4 ตำแหน่ง เพื่อจัดกลุ่มจุดที่ใกล้เคียงกันมากๆ)
		cacheKey := fmt.Sprintf("geo:nearby:%.4f:%.4f:%.0f", lng, lat, radius)

		// 3. เช็ค Redis (Cache HIT)
		if rdb != nil {
			cachedData, err := rdb.Get(ctx, cacheKey).Result()
			if err == nil {
				c.Set("Content-Type", "application/json")
				c.Set("X-Cache", "HIT")
				return c.SendString(cachedData)
			}
		}

		// 4. ถ้าไม่มีใน Cache (Cache MISS) ไปค้นหาใน MongoDB
		if mongoDb == nil {
			return c.Status(500).JSON(fiber.Map{"error": "MongoDB not connected"})
		}

		coll := mongoDb.Collection("customers")
		filter := bson.M{
			"location": bson.M{
				"$nearSphere": bson.M{
					"$geometry": bson.M{
						"type":        "Point",
						"coordinates": []float64{lng, lat},
					},
					"$maxDistance": radius, // ค้นหาในระยะที่กำหนด
				},
			},
		}

		// จำกัดผลลัพธ์แค่ 100 รายการ เพื่อป้องกัน Payload ใหญ่เกินไป
		findOptions := options.Find().SetLimit(100)

		cursor, err := coll.Find(ctx, filter, findOptions)
		if err != nil {
			log.Printf("❌ MongoDB Query Error: %v", err)
			return c.Status(500).JSON(fiber.Map{"error": "Database query failed"})
		}
		defer cursor.Close(ctx)

		var results []bson.M
		if err = cursor.All(ctx, &results); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to parse results"})
		}

		if results == nil {
			results = []bson.M{} // ป้องกันการคืนค่า null
		}

		// 5. บันทึกผลลัพธ์ลง Redis (TTL 5 นาที)
		resultsJSON, _ := json.Marshal(results)
		if rdb != nil {
			rdb.Set(ctx, cacheKey, resultsJSON, 5*time.Minute)
		}

		c.Set("Content-Type", "application/json")
		c.Set("X-Cache", "MISS")
		return c.Send(resultsJSON)
	})

	// (โค้ดเก่า) Flow 3.3: อ่านข้อมูลลูกค้าตาม ID
	app.Get("/api/customers/:id", func(c *fiber.Ctx) error {
		// ... โค้ดเดิม ... (ผมคงไว้ให้ในไฟล์เต็มนี้แล้วเพื่อความสมบูรณ์)
		id := c.Params("id")
		cacheKey := "customer:" + id

		if rdb != nil {
			cachedData, err := rdb.Get(ctx, cacheKey).Result()
			if err == nil {
				c.Set("Content-Type", "application/json")
				c.Set("X-Cache", "HIT")
				return c.SendString(cachedData)
			}
		}

		if mongoDb == nil {
			return c.Status(500).JSON(fiber.Map{"error": "MongoDB not connected"})
		}
		var customer bson.M
		coll := mongoDb.Collection("customers")
		err := coll.FindOne(ctx, bson.M{"_id": id}).Decode(&customer)
		if err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "Customer not found"})
		}

		custJSON, _ := json.Marshal(customer)
		if rdb != nil {
			rdb.Set(ctx, cacheKey, custJSON, 15*time.Minute)
		}

		c.Set("X-Cache", "MISS")
		return c.JSON(customer)
	})

	// (โค้ดเก่า) Flow 3.1: ประมวลผลเชิงพื้นที่ (Snap)
	app.Post("/api/spatial/snap", func(c *fiber.Ctx) error {
		// ... โค้ดเดิม ...
		reqBody := c.Body()
		hash := sha256.Sum256(reqBody)
		cacheKey := "spatial:snap:" + hex.EncodeToString(hash[:16])

		if rdb != nil {
			cachedSnap, err := rdb.Get(ctx, cacheKey).Result()
			if err == nil {
				c.Set("Content-Type", "application/json")
				c.Set("X-Cache", "HIT")
				return c.SendString(cachedSnap)
			}
		}

		resp, err := http.Post(pythonURL+"/snap", "application/json", bytes.NewBuffer(reqBody))
		if err != nil || resp.StatusCode != 200 {
			return c.Status(502).JSON(fiber.Map{"error": "Failed to call Python service"})
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)

		if pgPool != nil {
			query := `INSERT INTO geometries (geom) VALUES (ST_SetSRID(ST_GeomFromGeoJSON($1), 4326))`
			_, pgErr := pgPool.Exec(ctx, query, string(reqBody))
			if pgErr != nil {
				log.Printf("⚠️ PostGIS Insert Warning: %v", pgErr)
			}
		}

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