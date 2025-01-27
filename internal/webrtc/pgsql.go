package webrtc

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Streamer struct {
	Name string `db:name`
	AuthToken string `db:auth_token`
	StreamKey string
}

func GetStreamKeys(pool *pgxpool.Pool, ctx context.Context) ([]string, error) {
	query := `SELECT DISTINCT(unnest(stream_key)) FROM streamers`
	rows, _ := pool.Query(ctx, query)
	var streamKeys []string
	for rows.Next() {
		var streamKey string
		rows.Scan(&streamKey)
		streamKeys = append(streamKeys, streamKey)
	}
	return streamKeys, nil
}


func NewStreamer(pool *pgxpool.Pool, ctx context.Context, token []string) *Streamer{
	query := `SELECT name,auth_token FROM streamers
		 WHERE @streamKey = ANY(stream_key)
		 AND auth_token = @authToken`
	row := pool.QueryRow(ctx, query, pgx.NamedArgs{
		"streamKey": token[0],
		"authToken": token[1],
	})
	s := new(Streamer)
	err := row.Scan(&s.Name, &s.AuthToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "QueryRow failed: %v\n", err)
		return nil
	}
	s.StreamKey = token[0]

	return s
}
