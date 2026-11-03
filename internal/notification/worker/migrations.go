package worker

import (
	"context"
	"database/sql"
	"fmt"
)

const currentSchema = 10

func StorageSchemaVersion() int { return currentSchema }

func supportedSnapshotSchema(version int) bool { return version >= 6 && version <= currentSchema }

// Migration runs under the installation's exclusive instance lease. Each schema
// version is committed with its DDL; an older binary refuses the upgraded state.
func (s *Store) migrate() error {
	return s.transaction(context.Background(), s.migrateTransaction)
}

func (s *Store) migrateTransaction(tx *sql.Tx) error {
	if err := s.migrateThroughLED(tx); err != nil {
		return err
	}
	if err := verifyAdministrativeHistory(context.Background(), tx, s.identity); err != nil {
		return err
	}
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == currentSchema {
		return nil
	}
	if version != 9 {
		return fmt.Errorf("incomplete notification schema migration")
	}
	// Version 10 fences older runtimes/recovery helpers that do not preserve
	// administrative audit and its independent intent/completion evidence.
	_, err := tx.Exec("PRAGMA user_version=10")
	return err
}

func (s *Store) migrateThroughLED(tx *sql.Tx) error {
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == 9 || version == currentSchema {
		if err := checkLEDLayout(context.Background(), tx, s.config, s.identity, true); err != nil {
			return err
		}
		if err := checkLEDOperations(context.Background(), tx, s.identity); err != nil {
			return err
		}
		return checkLEDMigrations(context.Background(), tx, s.config, s.identity, false)
	}
	if version == 8 {
		if err := checkLEDLayout(context.Background(), tx, s.config, s.identity, true); err != nil {
			return err
		}
		if err := checkLEDLegacyMapping(context.Background(), tx); err != nil {
			return err
		}
		if err := checkLEDOperations(context.Background(), tx, s.identity); err != nil {
			return err
		}
		return migrateLEDMappings(tx)
	}
	if version == 7 {
		if err := checkLEDLegacyMapping(context.Background(), tx); err != nil {
			return err
		}
		if err := checkLEDLayout(context.Background(), tx, s.config, s.identity, true); err != nil {
			return err
		}
		return migrateLEDOperations(tx)
	}
	if version == 6 {
		return s.migrateLED(tx)
	}
	if version == 4 || version == 5 {
		return s.migrateLED(tx)
	}
	if version < 1 || version > 3 {
		return fmt.Errorf("unsupported notification schema")
	}
	if version == 1 {
		if _, err := tx.Exec(`
CREATE TABLE fault_transitions(producer TEXT NOT NULL,id TEXT NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL,PRIMARY KEY(producer,id));
CREATE INDEX transition_pending ON fault_transitions(producer,state);
CREATE TABLE policy_faults(producer TEXT NOT NULL,subject TEXT NOT NULL,revision INTEGER NOT NULL,condition TEXT NOT NULL,active INTEGER NOT NULL,PRIMARY KEY(producer,subject,revision));
CREATE TABLE policy_support(producer TEXT NOT NULL,id TEXT NOT NULL,revision INTEGER NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(producer,id));
CREATE TABLE interaction_contexts(producer TEXT NOT NULL,id TEXT NOT NULL,revision INTEGER NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(producer,id,revision));
PRAGMA user_version=2;`); err != nil {
			return err
		}
	}
	if version < 3 {
		_, err := tx.Exec(`
CREATE TABLE command_inbox(request_key TEXT PRIMARY KEY,input_hash TEXT NOT NULL,command_id TEXT NOT NULL);
CREATE TABLE commands(id TEXT PRIMARY KEY,request_key TEXT NOT NULL UNIQUE,input_hash TEXT NOT NULL,producer TEXT NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL,imported INTEGER NOT NULL DEFAULT 0,grant TEXT NOT NULL DEFAULT '',revision INTEGER NOT NULL DEFAULT 0,preparation TEXT NOT NULL DEFAULT '',confirmed INTEGER NOT NULL DEFAULT 0,token_hash TEXT UNIQUE,conflict INTEGER NOT NULL DEFAULT 0,created INTEGER NOT NULL);
CREATE INDEX command_queue ON commands(producer,state,created);
ALTER TABLE commands ADD COLUMN non_admission TEXT NOT NULL DEFAULT '';
CREATE TABLE command_results(command_id TEXT NOT NULL,revision INTEGER NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL,receipt TEXT NOT NULL,acked INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(command_id,revision));
CREATE TABLE command_replies(command_id TEXT NOT NULL,kind TEXT NOT NULL,revision INTEGER NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'pending',PRIMARY KEY(command_id,kind,revision));
ALTER TABLE command_replies ADD COLUMN attempt TEXT NOT NULL DEFAULT '';
ALTER TABLE command_replies ADD COLUMN tries INTEGER NOT NULL DEFAULT 0;
ALTER TABLE command_replies ADD COLUMN due INTEGER NOT NULL DEFAULT 0;
CREATE TABLE command_reply_attempts(id TEXT PRIMARY KEY,command_id TEXT NOT NULL,kind TEXT NOT NULL,revision INTEGER NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL,native_id TEXT NOT NULL DEFAULT '');
CREATE TABLE telegram_cursors(account TEXT PRIMARY KEY,offset INTEGER NOT NULL);
CREATE TABLE provider_inbox(account TEXT NOT NULL,id INTEGER NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(account,id));
CREATE TABLE command_rate(provider TEXT NOT NULL,account TEXT NOT NULL,actor TEXT NOT NULL,credit INTEGER NOT NULL,updated INTEGER NOT NULL,PRIMARY KEY(provider,account,actor));
CREATE TABLE signal_imports(account TEXT PRIMARY KEY,cursor TEXT NOT NULL);
CREATE TABLE policy_dependencies(producer TEXT NOT NULL,subject TEXT NOT NULL,decision TEXT NOT NULL,revision INTEGER NOT NULL,PRIMARY KEY(producer,subject,decision));
PRAGMA user_version=3;`)
		if err != nil {
			return err
		}
	}
	_, err := tx.Exec(`
CREATE TABLE notification_references(reference TEXT PRIMARY KEY,producer TEXT NOT NULL,epoch TEXT NOT NULL,generation TEXT NOT NULL,body TEXT NOT NULL);
CREATE TABLE notification_deliveries(destination TEXT NOT NULL,native_id TEXT NOT NULL,sender TEXT NOT NULL,reference TEXT NOT NULL,expires INTEGER NOT NULL,conflict INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(destination,native_id,sender));
CREATE INDEX notification_reference_lookup ON notification_deliveries(reference,destination);
CREATE TABLE command_aliases(alias TEXT PRIMARY KEY,producer TEXT NOT NULL,epoch TEXT NOT NULL);
CREATE TABLE gateway_queries(id TEXT PRIMARY KEY,body TEXT NOT NULL);
CREATE TABLE gateway_pages(token TEXT PRIMARY KEY,root TEXT NOT NULL,position INTEGER NOT NULL,UNIQUE(root,position));
CREATE TABLE command_admissions(request_key TEXT PRIMARY KEY,provider TEXT NOT NULL,account TEXT NOT NULL,actor TEXT NOT NULL,conversation TEXT NOT NULL,created INTEGER NOT NULL);
CREATE INDEX command_admission_window ON command_admissions(created);
INSERT INTO command_admissions SELECT request_key,json_extract(body,'$.destination.provider'),json_extract(body,'$.destination.account'),json_extract(body,'$.actor'),json_extract(body,'$.destination.id'),created FROM commands;
CREATE INDEX reply_native_lookup ON command_reply_attempts(native_id,state);
PRAGMA user_version=6;`)
	if err != nil {
		return err
	}
	return s.migrateLED(tx)
}
