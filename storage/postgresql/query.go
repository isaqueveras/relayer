package postgresql

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/nbd-wtf/go-nostr"
)

func (b PostgresBackend) QueryEvents(ctx context.Context, filter *nostr.Filter) (ch chan *nostr.Event, err error) {
	ch = make(chan *nostr.Event)

	query, params, err := b.queryEventsSql(filter, false)
	if err != nil {
		close(ch)
		return nil, err
	}

	rows, err := b.DB.Query(query, params...)
	if err != nil && err != sql.ErrNoRows {
		close(ch)
		return nil, fmt.Errorf("failed to fetch events using query %q: %w", query, err)
	}

	go func() {
		defer rows.Close()
		defer close(ch)
		for rows.Next() {
			var evt nostr.Event
			var timestamp int64
			err := rows.Scan(&evt.ID, &evt.PubKey, &timestamp,
				&evt.Kind, &evt.Tags, &evt.Content, &evt.Sig)
			if err != nil {
				return
			}
			evt.CreatedAt = nostr.Timestamp(timestamp)
			ch <- &evt
		}
	}()

	return ch, nil
}

func (b PostgresBackend) CountEvents(ctx context.Context, filter *nostr.Filter) (int64, error) {
	query, params, err := b.queryEventsSql(filter, true)
	if err != nil {
		return 0, err
	}

	var count int64
	if err = b.DB.QueryRow(query, params...).Scan(&count); err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("failed to fetch events using query %q: %w", query, err)
	}
	return count, nil
}

func (b PostgresBackend) queryEventsSql(filter *nostr.Filter, doCount bool) (string, []any, error) {
	var conditions []string
	var params []any

	if filter == nil {
		return "", nil, fmt.Errorf("filter cannot be null")
	}

	if filter.IDs != nil {
		if len(filter.IDs) > b.QueryIDsLimit {
			// too many ids, fail everything
			return "", nil, nil
		}

		likeids := make([]string, 0, len(filter.IDs))
		for _, id := range filter.IDs {
			// to prevent sql attack here we will check if
			// these ids are valid 32byte hex
			parsed, err := hex.DecodeString(id)
			if err != nil || len(parsed) != 32 {
				continue
			}
			likeids = append(likeids, fmt.Sprintf("id LIKE '%x%%'", parsed))
		}
		if len(likeids) == 0 {
			// ids being [] mean you won't get anything
			return "", nil, nil
		}
		conditions = append(conditions, "("+strings.Join(likeids, " OR ")+")")
	}

	if filter.Authors != nil {
		if len(filter.Authors) > b.QueryAuthorsLimit {
			// too many authors, fail everything
			return "", nil, nil
		}

		likekeys := make([]string, 0, len(filter.Authors))
		for _, key := range filter.Authors {
			// to prevent sql attack here we will check if
			// these keys are valid 32byte hex
			parsed, err := hex.DecodeString(key)
			if err != nil || len(parsed) != 32 {
				continue
			}
			likekeys = append(likekeys, fmt.Sprintf("pubkey LIKE '%x%%'", parsed))
		}
		if len(likekeys) == 0 {
			// authors being [] mean you won't get anything
			return "", nil, nil
		}
		conditions = append(conditions, "("+strings.Join(likekeys, " OR ")+")")
	}

	if filter.Kinds != nil {
		if len(filter.Kinds) > b.QueryKindsLimit {
			// too many kinds, fail everything
			return "", nil, nil
		}

		if len(filter.Kinds) == 0 {
			// kinds being [] mean you won't get anything
			return "", nil, nil
		}
		// no sql injection issues since these are ints
		inkinds := make([]string, len(filter.Kinds))
		for i, kind := range filter.Kinds {
			inkinds[i] = strconv.Itoa(kind)
		}
		conditions = append(conditions, `kind IN (`+strings.Join(inkinds, ",")+`)`)
	}

	tagQuery := make([]string, 0, 1)
	for _, values := range filter.Tags {
		if len(values) == 0 {
			// any tag set to [] is wrong
			return "", nil, nil
		}

		// add these tags to the query
		tagQuery = append(tagQuery, values...)

		if len(tagQuery) > b.QueryTagsLimit {
			// too many tags, fail everything
			return "", nil, nil
		}
	}

	if len(tagQuery) > 0 {
		arrayBuild := make([]string, len(tagQuery))
		for i, tagValue := range tagQuery {
			arrayBuild[i] = "?"
			params = append(params, tagValue)
		}

		// we use a very bad implementation in which we only check the tag values and
		// ignore the tag names
		conditions = append(conditions,
			"tagvalues && ARRAY["+strings.Join(arrayBuild, ",")+"]")
	}

	if filter.Since != nil {
		conditions = append(conditions, "created_at >= ?")
		params = append(params, filter.Since)
	}
	if filter.Until != nil {
		conditions = append(conditions, "created_at <= ?")
		params = append(params, filter.Until)
	}

	if len(conditions) == 0 {
		// fallback
		conditions = append(conditions, "true")
	}

	if filter.Limit < 1 || filter.Limit > b.QueryLimit {
		params = append(params, b.QueryLimit)
	} else {
		params = append(params, filter.Limit)
	}

	var query string
	if doCount {
		query = sqlx.Rebind(sqlx.BindType("postgres"), `SELECT
          COUNT(*)
        FROM event WHERE `+
			strings.Join(conditions, " AND ")+
			" ORDER BY created_at DESC LIMIT ?")
	} else {
		query = sqlx.Rebind(sqlx.BindType("postgres"), `SELECT
          id, pubkey, created_at, kind, tags, content, sig
        FROM event WHERE `+
			strings.Join(conditions, " AND ")+
			" ORDER BY created_at DESC LIMIT ?")
	}

	return query, params, nil
}
