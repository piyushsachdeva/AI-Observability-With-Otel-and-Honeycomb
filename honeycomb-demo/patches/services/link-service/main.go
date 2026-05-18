package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
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
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"

	appOtel "github.com/itsbaivab/url-shortener/internal/otel"
)

const serviceName = "link-service"

type LinkServiceHandler struct {
	linkService *services.LinkService
}

type CreateLinkRequest struct {
	Long string `json:"long" binding:"required"`
}

type DeleteLinkRequest struct {
	ID string `json:"id" binding:"required"`
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
	linkService := services.NewLinkService(linkRepo, redisCache)

	handler := &LinkServiceHandler{linkService: linkService}

	router := gin.Default()
	router.Use(otelgin.Middleware(serviceName))

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "healthy", "service": serviceName})
	})
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))
	router.PUT("/generate", handler.CreateLink)
	router.GET("/links", handler.GetAllLinks)
	router.DELETE("/delete", handler.DeleteLink)

	port := getEnv("SERVICE_PORT", "8001")
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

func (h *LinkServiceHandler) CreateLink(c *gin.Context) {
	ctx, span := otel.Tracer(serviceName).Start(c.Request.Context(), "CreateLink")
	defer span.End()

	var req CreateLinkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		span.SetStatus(codes.Error, err.Error())
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if len(req.Long) < 15 {
		span.SetStatus(codes.Error, "url too short")
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL must be at least 15 characters long"})
		return
	}

	link := domain.Link{
		Id:          generateShortURLID(8),
		OriginalURL: req.Long,
		CreatedAt:   time.Now(),
	}

	span.SetAttributes(
		attribute.String("link.id", link.Id),
		attribute.String("link.original_url", req.Long),
		semconv.HTTPRouteKey.String("/generate"),
	)

	if err := h.linkService.Create(ctx, link); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	span.SetStatus(codes.Ok, "link created")
	c.JSON(http.StatusOK, link)
}

func (h *LinkServiceHandler) GetAllLinks(c *gin.Context) {
	ctx, span := otel.Tracer(serviceName).Start(c.Request.Context(), "GetAllLinks")
	defer span.End()

	links, err := h.linkService.GetAll(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		log.Printf("Error getting all links: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get links"})
		return
	}

	span.SetAttributes(attribute.Int("links.count", len(links)))
	c.JSON(http.StatusOK, links)
}

func (h *LinkServiceHandler) DeleteLink(c *gin.Context) {
	ctx, span := otel.Tracer(serviceName).Start(c.Request.Context(), "DeleteLink")
	defer span.End()

	var req DeleteLinkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		span.SetStatus(codes.Error, err.Error())
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	span.SetAttributes(attribute.String("link.id", req.ID))

	if err := h.linkService.Delete(ctx, req.ID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusNoContent, nil)
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func generateShortURLID(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	for i := range result {
		charIndex, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		result[i] = charset[charIndex.Int64()]
	}
	return string(result)
}
