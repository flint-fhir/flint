"""Query Iceberg table and print rows."""
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
print(f"Table: {table.name()}")
print(f"Schema: {table.schema()}")
print(f"Current Snapshot ID: {table.current_snapshot().snapshot_id}")

df = table.scan().to_arrow()
print(f"\nRow count: {len(df)}")
for col in df.column_names:
    print(f"  {col}: {df[col].to_pylist()}")
