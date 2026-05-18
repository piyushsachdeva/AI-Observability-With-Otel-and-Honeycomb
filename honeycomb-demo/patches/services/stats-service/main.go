package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/itsbaivab/url-shortener/internal/adapters/cache"
	"github.com/itsbaivab/url-shortener/internal/adapters/repository/postgres"
	"github.com/itsbaivab/url-shortener/internal/core/services"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	appOtel "github.com/itsbaivab/url-shortener/internal/otel"
)

const serviceName = "stats-service"

type StatsServiceHandler struct {
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

	handler := &StatsServiceHandler{
		linkService:  linkService,
		statsService: statsService,
	}

	router := gin.Default()
	router.Use(otelgin.Middleware(serviceName))

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "healthy", "service": serviceName})
	})
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))
	router.GET("/stats", handler.GetStats)
	router.GET("/stats/:id", handler.GetStatsByLinkID)

	port := getEnv("SERVICE_PORT", "8003")
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

func (h *StatsServiceHandler) GetStats(c *gin.Context) {
	ctx, span := otel.Tracer(serviceName).Start(c.Request.Context(), "GetStats")
	defer span.End()

	links, err := h.linkService.GetAll(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	for i, link := range links {
		stats, err := h.statsService.GetStatsByLinkID(ctx, link.Id)
		if err != nil {
			log.Printf("Error getting stats for link '%s': %v", link.Id, err)
			continue
		}
		links[i].Stats = stats
	}

	span.SetAttributes(attribute.Int("links.count", len(links)))
	c.JSON(http.StatusOK, links)
}

func (h *StatsServiceHandler) GetStatsByLinkID(c *gin.Context) {
	ctx, span := otel.Tracer(serviceName).Start(c.Request.Context(), "GetStatsByLinkID")
	defer span.End()

	linkID := c.Param("id")
	if linkID == "" {
		span.SetStatus(codes.Error, "missing link id")
		c.JSON(http.StatusBadRequest, gin.H{"error": "Link ID parameter is required"})
		return
	}

	span.SetAttributes(attribute.String("link.id", linkID))

	stats, err := h.statsService.GetStatsByLinkID(ctx, linkID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, stats)
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
