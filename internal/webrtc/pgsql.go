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
	StreamKey string
}

func LookupStreamer(conn *pgxpool.Conn, ctx context.Context, token []string) (*Streamer, bool){
	query := `SELECT name FROM streamers
		 WHERE @streamKey = ANY(stream_key)
		 AND auth_token = @authToken`
	row := conn.QueryRow(ctx, query, pgx.NamedArgs{
		"streamKey": token[0],
		"authToken": token[1],
	})
	streamer := new(Streamer)
	err := row.Scan(&streamer.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "QueryRow failed: %v\n", err)
		return nil, false
	}
	streamer.StreamKey = token[0]

	return streamer, true
}
