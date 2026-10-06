"""Query Iceberg table using DuckDB SQL."""
import duckdb
from pyiceberg.catalog import load_catalog

catalog = load_catalog(
    "flint",
    **{
        "type": "rest",
        "uri": "http://localhost:8181",
        "s3.endpoint": "http://localhost:9000",
        "s3.access-key-id": "GK1e8391735937d6afcd2dd84c",
        "s3.secret-access-key": "e9249c7fd2bbce0600a196bc6bc97f069b8948f20bf54fcaaefa924256b01e6c",
        "s3.path-style-access": "true",
        "s3.region": "us-east-1",
    }
)

table = catalog.load_table(("flint", "fhir.Patient"))
arrow_table = table.scan().to_arrow()

con = duckdb.connect()
con.register("patient_iceberg", arrow_table)

print("=== DuckDB SQL Query on Iceberg Table ===")
result = con.execute("""
    SELECT 
        _kafka_key AS patient_id,
        _kafka_metadata.partition AS partition,
        _kafka_metadata.offset AS offset,
        CAST(_kafka_value AS VARCHAR) AS raw_json
    FROM patient_iceberg
    ORDER BY _kafka_key
""").fetchdf()

print(result.to_string(index=False))
