/*
 * Copyright 2015 Google Inc. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/* TODO: should we consider the case where multiple declarations and multiple
 * definitions exist for the same symbol? This could happen before C
 * preprocesor */

package main

import (
    "database/sql"
    _ "github.com/mattn/go-sqlite3"
    "log"
    "bytes"
    "io"
    "crypto/sha1"
    "os"
)

type Symbol struct {
    name    string
    file    string
    line    int
    col     int
}

type SymbolsDB struct {
    db              *sql.DB

    insertFile      *sql.Stmt
    selectFileHash  *sql.Stmt
    insertSymb      *sql.Stmt
    selectSymb      *sql.Stmt
    delFileRef      *sql.Stmt
}

func (db *SymbolsDB) empty() bool {
    rows, err := db.db.Query(`SELECT name FROM sqlite_master
                            WHERE type='table' AND name='files';`)
    if err != nil {
        log.Fatal("check empty ", err)
    }
    defer rows.Close()

    return !rows.Next()
}

func (db *SymbolsDB) initDB() {
    initStmt := `
        CREATE TABLE files (
            id      INTEGER,
            hash    BLOB UNIQUE,
            path    TEXT UNIQUE,
            PRIMARY KEY(id)
        );
        CREATE TABLE symbol_decls (
            name    TEXT,
            file    INTEGER,
            line    INTEGER,
            col     INTEGER,
            PRIMARY KEY(name, file, line, col),
            FOREIGN KEY(file) REFERENCES files(id) ON DELETE CASCADE
        );
        CREATE TABLE func_defs (
            name        TEXT,
            file        INTEGER,
            line        INTEGER,
            col         INTEGER,

            def_file    INTEGER,
            def_line    INTEGER,
            def_col     INTEGER,

            PRIMARY KEY(name, file, line, col),
            FOREIGN KEY(file) REFERENCES files(id) ON DELETE CASCADE,

            FOREIGN KEY(name, def_file, def_line, def_col)
                REFERENCES symbol_decls(name, file, line, col) ON DELETE CASCADE
        );
    `
    _, err := db.db.Exec(initStmt)
    if err != nil {
        log.Fatal("init db ", err)
    }
}

func OpenSymbolsDB(path string) *SymbolsDB {
    db, err := sql.Open("sqlite3", path)
    if err != nil {
        log.Fatal("open db ", err)
    }

    r := &SymbolsDB{db: db}

    db.Exec(`PRAGMA foreign_keys = ON;`)

    if r.empty() {
        r.initDB()
    }

    r.insertFile, err = db.Prepare(`
        INSERT INTO files(path, hash) VALUES (?, ?);
    `)
    if err != nil {
        log.Fatal("prepare insert files ", err)
    }

    r.selectFileHash, err = db.Prepare(`
        SELECT hash FROM files WHERE path = ?;
    `)
    if err != nil {
        log.Fatal("prepare select hash ", err)
    }

    r.insertSymb, err = db.Prepare(`
        INSERT INTO symbol_decls(name, file, line, col)
            SELECT ?, id, ?, ? FROM files
            WHERE path = ?;
    `)
    if err != nil {
        log.Fatal("prepare insert symbol ", err)
    }

    r.selectSymb, err = db.Prepare(`
        SELECT name, path, line, col FROM symbol_decls, files
        WHERE name = ? AND id = file;
    `)
    if err != nil {
        log.Fatal("prepare select symbol ", err)
    }

    r.delFileRef, err = db.Prepare(`
        DELETE FROM files WHERE path = ?;
    `)
    if err != nil {
        log.Fatal("prepare delete file ", err)
    }

    return r
}

func (db *SymbolsDB) InsertSymbol(sym *Symbol) {
    _, err := db.insertSymb.Exec(sym.name, sym.line, sym.col, sym.file)
    if err != nil {
        log.Fatal("insert symbol ", err)
    }
}

func (db *SymbolsDB) GetSymbols(name string) []*Symbol {
    r, err := db.selectSymb.Query(name)
    if err != nil {
        log.Fatal("select symbol ", err)
    }
    defer r.Close()

    rs := make([]*Symbol, 0)
    for r.Next() {
        s := new(Symbol)

        err = r.Scan(&s.name, &s.file, &s.line, &s.col)
        if err != nil {
            log.Fatal("scan symbol ", err)
        }

        rs = append(rs, s)
    }

    return rs
}

func calculateSha1(file string) ([]byte, error) {
    f, err := os.Open(file)
    if err != nil {
        return nil, err
    }
    defer f.Close()

    hash := sha1.New()
    _, err = io.Copy(hash, f)
    if err != nil {
        return nil, err
    }

    return hash.Sum(nil), nil
}

/*
 * This function checks if the file exist and it is up to date. If it is not
 * not up to date, it will remove the current references of the file in the DB.
 * In either case, it will insert a new file entry in the DB and the Parser
 * should be called to populate the DB with the new symbols.
 */
func (db *SymbolsDB) NeedToProcessFile(file string) bool {
    // TODO: This can be paifully slow. Check if this is the case. If so, We
    // should make use of mtime as git does.
    // Check out:
    //  http://www-cs-students.stanford.edu/~blynn/gg/race.html
    //  https://www.kernel.org/pub/software/scm/git/docs/technical/racy-git.txt
    hash, err := calculateSha1(file)
    if err != nil {
        log.Println(err, ": unable to read file ", file)
        return false
    }

    r, err := db.selectFileHash.Query(file)
    if err != nil {
        log.Fatal("select file hash ", err)
    }
    defer r.Close()

    if r.Next() {
        var inDbHash []byte

        err := r.Scan(&inDbHash)
        if err != nil {
            log.Fatal("scanning hash ", err)
        }

        if bytes.Compare(hash, inDbHash) == 0 {
            // the hash in the DB and the file are the same; nothing to process.
            return false
        } else {
            // not up to date, remove all references
            db.RemoveFileReferences(file)
        }
    }

    _, err = db.insertFile.Exec(file, hash)
    if err != nil {
        log.Fatal("insert file ", err)
    }

    return true
}

func (db *SymbolsDB) RemoveFileReferences(file string) {
    _, err := db.delFileRef.Exec(file)
    if err != nil {
        log.Fatal("delete file ", err)
    }
}

func (db *SymbolsDB) GetSetFilesInDB() map[string]bool {
    rows, err := db.db.Query(`SELECT path FROM files;`)
    if err != nil {
        log.Fatal("select files ", err)
    }
    defer rows.Close()

    fileSet := map[string]bool{}
    for rows.Next() {
        var path string

        err := rows.Scan(&path)
        if err != nil {
            log.Fatal("scan path ", err)
        }

        fileSet[path] = true
    }

    return fileSet
}

func (db *SymbolsDB) Close() {
    db.insertFile.Close()
    db.selectFileHash.Close()
    db.insertSymb.Close()
    db.selectSymb.Close()
    db.delFileRef.Close()
    db.db.Close()
}