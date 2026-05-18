package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/itsbaivab/url-shortener/internal/adapters/cache"
	"github.com/itsbaivab/url-shortener/internal/adapters/repository/postgres"
	"github.com/itsbaivab/url-shortener/internal/core/domain"
	"github.com/itsbaivab/url-shortener/internal/core/services"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	appOtel "github.com/itsbaivab/url-shortener/internal/otel"
)

const serviceName = "redirect-service"

type RedirectServiceHandler struct {
	linkService  *services.LinkService
	statsService *services.StatsService
}

func main() {
	ctx := context.Background()

	shutdown, err := appOtel.Init(ctx, serviceName)
	if err != nil {
		log.Printf("Warning: OTel init failed: %v — continuing without tracing", err)
	} else {
		defer shutdown()
	}

	dbHost := getEnv("DB_HOST", "localhost")
	dbPort := getEnv("DB_PORT", "5432")
	dbUser := getEnv("DB_USER", "postgres")
	dbPassword := getEnv("DB_PASSWORD", "postgres")
	dbName := getEnv("DB_NAME", "urlshortener")

	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		dbHost, dbPort, dbUser, dbPassword, dbName)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatal("Failed to ping database:", err)
	}

	redisHost := getEnv("REDIS_HOST", "localhost")
	redisPort := getEnv("REDIS_PORT", "6379")
	redisCache := cache.NewRedisCache(redisHost+":"+redisPort, "", 0)

	linkRepo := postgres.NewPostgresLinkRepository(db)
	statsRepo := postgres.NewPostgresStatsRepository(db)
	linkService := services.NewLinkService(linkRepo, redisCache)
	statsService := services.NewStatsService(statsRepo, redisCache)

	handler := &RedirectServiceHandler{
		linkService:  linkService,
		statsService: statsService,
	}

	router := gin.Default()
	router.Use(otelgin.Middleware(serviceName))

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "healthy", "service": serviceName})
	})
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))
	router.GET("/redirect/:id", handler.Redirect)

	port := getEnv("SERVICE_PORT", "8002")
	srv := &http.Server{Addr: ":" + port, Handler: router}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()
	log.Printf("%s started on port %s", serviceName, port)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Printf("Shutting down %s...", serviceName)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}
	log.Printf("%s stopped", serviceName)
}

func (h *RedirectServiceHandler) Redirect(c *gin.Context) {
	ctx, span := otel.Tracer(serviceName).Start(c.Request.Context(), "Redirect")
	defer span.End()

	id := c.Param("id")
	if id == "" {
		span.SetStatus(codes.Error, "missing id")
		c.JSON(http.StatusBadRequest, gin.H{"error": "ID parameter is required"})
		return
	}

	span.SetAttributes(attribute.String("link.id", id))

	// Feature flag: inject artificial latency to simulate a degraded service.
	// Toggle by updating the feature-flags ConfigMap (see inject-chaos.sh).
	if ms := chaosLatencyMs(); ms > 0 {
		span.SetAttributes(
			attribute.Int("chaos.latency_ms", ms),
			attribute.Bool("chaos.active", true),
		)
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}

	originalURL, err := h.linkService.GetOriginalURL(ctx, id)
	if err != nil || originalURL == nil || *originalURL == "" {
		span.SetStatus(codes.Error, "link not found")
		c.JSON(http.StatusNotFound, gin.H{"error": "Link not found"})
		return
	}

	span.SetAttributes(attribute.String("link.original_url", *originalURL))

	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		stats := domain.Stats{
			Id:        uuid.New().String(),
			LinkID:    id,
			Platform:  domain.PlatformUnknown,
			CreatedAt: time.Now(),
		}
		if err := h.statsService.Create(bgCtx, stats); err != nil {
			log.Printf("Failed to create stats: %v", err)
		}
	}()

	span.SetStatus(codes.Ok, "redirecting")
	c.Redirect(http.StatusMovedPermanently, *originalURL)
}

// chaosLatencyMs reads the CHAOS_LATENCY_MS env var (set from the feature-flags ConfigMap).
func chaosLatencyMs() int {
	v := os.Getenv("CHAOS_LATENCY_MS")
	if v == "" || v == "0" {
		return 0
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms < 0 {
		return 0
	}
	return ms
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
