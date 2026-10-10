lint {
  destructive {
    error = true
  }
  data_depend {
    error = true
  }
  naming {
    match   = "^[a-z][a-z0-9_]*$"
    message = "must be lowercase snake_case"
  }
}

env "local" {
  src = "file://store/postgres/schema.sql"
  url = getenv("FLINT_TEST_POSTGRES_DSN")
  dev = getenv("FLINT_DEV_POSTGRES_DSN")
  migration {
    dir = "file://store/postgres/migrations"
  }
}

env "ci" {
  src = "file://store/postgres/schema.sql"
  url = "postgres://flint:flint@localhost:5432/flint?sslmode=disable"
  dev = "postgres://flint:flint@localhost:5432/flint?sslmode=disable&search_path=atlas_dev"
  migration {
    dir = "file://store/postgres/migrations"
  }
}

env "deployed" {
  src = "file://store/postgres/schema.sql"
  url = "postgres://${urlescape(getenv("DATABASE_USER"))}:${urlescape(getenv("DATABASE_PASSWORD"))}@${getenv("DATABASE_HOST")}:${getenv("DATABASE_PORT")}/${getenv("DATABASE_NAME")}?search_path=${urlescape(getenv("DATABASE_SCHEMA"))}&sslmode=${getenv("DATABASE_SSLMODE")}"
  migration {
    dir = "file://store/postgres/migrations"
  }
}
