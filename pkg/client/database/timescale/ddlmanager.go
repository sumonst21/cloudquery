package timescale

import (
	"context"
	"fmt"

	"github.com/cloudquery/cloudquery/pkg/client/history"
	"github.com/cloudquery/cq-provider-sdk/provider/schema"
	"github.com/georgysavva/scany/pgxscan"
	"github.com/hashicorp/go-hclog"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
)

const (
	listHyperTables = `SELECT hypertable_name FROM timescaledb_information.hypertables WHERE hypertable_schema=$1 ORDER BY 1`

	setChunkTimeInterval = `SELECT * FROM set_chunk_time_interval($1, INTERVAL '%d hour');`
	dataRetentionPolicy  = `SELECT history.update_retention($1, INTERVAL '%d day');`

	dropTableView   = `DROP VIEW IF EXISTS "%[1]s"`
	createTableView = `CREATE VIEW "%[1]s" AS SELECT * FROM history."%[1]s" WHERE cq_fetch_date = find_latest('history', '%[1]s')`
)

type DDLManager struct {
	log     hclog.Logger
	pool    *pgxpool.Pool
	cfg     *history.Config
	dialect schema.Dialect
}

func NewDDLManager(l hclog.Logger, pool *pgxpool.Pool, cfg *history.Config, dt schema.DialectType) (*DDLManager, error) {
	if dt != schema.TSDB {
		return nil, fmt.Errorf("history is only supported on timescaledb")
	}

	dialect, err := schema.GetDialect(dt)
	if err != nil {
		return nil, err
	}

	return &DDLManager{
		log:     l,
		pool:    pool,
		cfg:     cfg,
		dialect: dialect,
	}, nil
}

func (d *DDLManager) Close() {
	if d.pool != nil {
		d.pool.Close()
	}
}

// SetupHistory is run after any migrations, finalizing history setup
func (d *DDLManager) SetupHistory(ctx context.Context) error {
	var tables []string
	if err := pgxscan.Select(ctx, d.pool, &tables, listHyperTables, history.SchemaName); err != nil {
		return fmt.Errorf("failed to list hypertables: %w", err)
	}

	for _, table := range tables {
		if err := d.configureHyperTable(ctx, table); err != nil {
			return fmt.Errorf("failed to configure hypertable for table: %s: %w", table, err)
		}
		if err := d.recreateView(ctx, table); err != nil {
			return fmt.Errorf("recreateView: %w", err)
		}
	}

	return nil
}

// DropViews drops all hypertable related views. This must be called before any migrations can be run.
func (d *DDLManager) DropViews(ctx context.Context) error {
	var tables []string
	if err := pgxscan.Select(ctx, d.pool, &tables, listHyperTables, history.SchemaName); err != nil {
		return fmt.Errorf("failed to list hypertables: %w", err)
	}

	if err := d.pool.BeginTxFunc(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		for _, table := range tables {
			if _, err := tx.Exec(ctx, fmt.Sprintf(dropTableView, table)); err != nil {
				return fmt.Errorf("failed to drop view for table: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (d *DDLManager) AddHistoryFunctions(ctx context.Context) error {
	return d.pool.BeginFunc(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, createHistorySchema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, setupTriggerFunction); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, setupParentFunction); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, defineRetentionFunction); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, cascadeDeleteFunction); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, findLatestFetchDate); err != nil {
			return err
		}
		return nil
	})
}

func (d *DDLManager) configureHyperTable(ctx context.Context, tableName string) error {
	tName := fmt.Sprintf(`"%s"."%s"`, history.SchemaName, tableName)

	if _, err := d.pool.Exec(ctx, fmt.Sprintf(setChunkTimeInterval, d.cfg.TimeInterval), tName); err != nil {
		return err
	}
	d.log.Debug("updated chunk_time_interval for table", "table", tableName, "interval", d.cfg.TimeInterval)

	// Below call is only needed for "parent" tables. dataRetentionPolicy function takes care of that by updating retention ONLY IF a previous retention policy is set.
	if _, err := d.pool.Exec(ctx, fmt.Sprintf(dataRetentionPolicy, d.cfg.Retention), tName); err != nil {
		return err
	}

	d.log.Debug("created data retention policy", "table", tableName, "days", d.cfg.Retention)
	return nil
}

func (d *DDLManager) recreateView(ctx context.Context, table string) error {
	if err := d.pool.BeginTxFunc(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Must drop the view first -- CREATE OR REPLACE view won't cut it if columns are changed. PostgreSQL doc states:
		// > The new query must generate the same columns that were generated by the existing view query (that is, the same column names in the same order and with
		// > the same data types), but it may add additional columns to the end of the list.
		// ref: https://www.postgresql.org/docs/14/sql-createview.html

		if _, err := tx.Exec(ctx, fmt.Sprintf(dropTableView, table)); err != nil {
			return fmt.Errorf("failed to drop view for table: %w", err)
		}

		if _, err := tx.Exec(ctx, fmt.Sprintf(createTableView, table)); err != nil {
			return fmt.Errorf("failed to create view for table: %w", err)
		}

		return nil
	}); err != nil {
		return fmt.Errorf("tx failed for %s: %w", table, err)
	}
	return nil
}

const (
	createHistorySchema   = `CREATE SCHEMA IF NOT EXISTS history;`
	cascadeDeleteFunction = `
				CREATE OR REPLACE FUNCTION history.cascade_delete()
					RETURNS trigger
					LANGUAGE 'plpgsql'
					COST 100
					VOLATILE NOT LEAKPROOF
				AS $BODY$
				BEGIN
					BEGIN
						IF (TG_OP = 'DELETE') THEN
							EXECUTE format('DELETE FROM history.%I where %I = %L AND cq_fetch_date = %L', TG_ARGV[0], TG_ARGV[1], OLD.cq_id, OLD.cq_fetch_date);
							RETURN OLD;
						END IF;
						RETURN NULL; -- result is ignored since this is an AFTER trigger
					END;
				END;
				$BODY$;`

	// Creates trigger on a referenced table, so each time a row from the parent table is deleted, referencing (child) rows are also cleared from database.
	setupTriggerFunction = `
				CREATE OR REPLACE FUNCTION history.setup_tsdb_child(_table_name text, _column_name text, _parent_table_name text, _parent_column_name text)
					RETURNS integer
					LANGUAGE 'plpgsql'
					COST 100
					VOLATILE PARALLEL UNSAFE
				AS $BODY$
				BEGIN
					PERFORM public.create_hypertable(_table_name, 'cq_fetch_date', chunk_time_interval => INTERVAL '1 day', if_not_exists => true);

					IF NOT EXISTS ( SELECT 1 FROM pg_trigger WHERE tgname = _table_name )  then
					EXECUTE format(
						'CREATE TRIGGER %I BEFORE DELETE ON history.%I FOR EACH ROW EXECUTE PROCEDURE history.cascade_delete(%s, %s)'::text,
						_table_name, _parent_table_name, _table_name, _column_name);
					return 0;
					ELSE
						return 1;
					END IF;
				END;
				$BODY$;`

	// Creates hypertable on the given table with a default chunk_time_interval, and adds a default retention policy
	setupParentFunction = `
				CREATE OR REPLACE FUNCTION history.setup_tsdb_parent(_table_name text)
					RETURNS integer
					LANGUAGE 'plpgsql'
					COST 100
					VOLATILE PARALLEL UNSAFE
				AS $BODY$
				DECLARE
					result integer;
				BEGIN
					PERFORM public.create_hypertable(_table_name, 'cq_fetch_date', chunk_time_interval => INTERVAL '1 day', if_not_exists => true);
					SELECT public.add_retention_policy(_table_name, INTERVAL '14 day', if_not_exists => true) into result;
					RETURN result;
				END;
				$BODY$;`

	// Updates the retention policy on the given table, only if a policy already exists.
	defineRetentionFunction = `
				CREATE OR REPLACE FUNCTION history.update_retention(_table_name text, _retention interval)
					RETURNS integer
					LANGUAGE 'plpgsql'
					COST 100
					VOLATILE PARALLEL UNSAFE
				AS $BODY$
				DECLARE
					result integer;
				BEGIN
					IF EXISTS ( SELECT 1 FROM timescaledb_information.jobs WHERE proc_name = 'policy_retention' AND hypertable_name = _table_name) THEN
						PERFORM remove_retention_policy(_table_name, if_exists => true);
						SELECT add_retention_policy(_table_name, _retention, if_not_exists => true) INTO result;
						RETURN result;
					ELSE
						RETURN -2;
					END IF;
				END;
				$BODY$;`

	findLatestFetchDate = `
			CREATE OR REPLACE FUNCTION find_latest(schema TEXT, _table_name TEXT) 
			RETURNS timestamp without time zone AS $body$
			DECLARE
			 fetchDate timestamp without time zone;
			BEGIN
				EXECUTE format('SELECT cq_fetch_date FROM %I.%I order by cq_fetch_date desc limit 1', schema, _table_name) into fetchDate;
				return fetchDate;
			END;
			$body$  LANGUAGE plpgsql IMMUTABLE`
)
