# Example precanned catalog. Set BAO_SQLITE_FDB_CATALOG to this file to load it.
#
# Consumers read a registered query at <mount>/query/<name> (the default
# database) or <mount>/query/<db>/<name>, and write through a registered exec at
# <mount>/exec/<db>/<name>. Params named in `args` bind positionally from the
# request `data` map in order. A schema block runs when its database opens.

query "greeting_by_lang" {
  sql  = "SELECT text FROM greetings WHERE lang = ?"
  args = ["lang"]
}

query "all_greetings" {
  sql = "SELECT lang, text FROM greetings"
}

exec "add_greeting" {
  sql  = "INSERT INTO greetings (lang, text) VALUES (?, ?)"
  args = ["lang", "text"]
}

schema "greetings_*" {
  sql = "CREATE TABLE IF NOT EXISTS greetings (lang TEXT PRIMARY KEY, text TEXT NOT NULL)"
}
