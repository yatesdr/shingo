//go:build deps

package shingocore

// Force module dependencies for packages used across the project.
import (
	_ "github.com/eclipse/paho.mqtt.golang"
	_ "github.com/go-chi/chi/v5"
	_ "github.com/google/uuid"
	_ "github.com/gorilla/sessions"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/redis/go-redis/v9"
	_ "github.com/segmentio/kafka-go"
	_ "golang.org/x/crypto/bcrypt"
	_ "gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)
