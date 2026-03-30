#!/bin/bash
for i in 1 2 3 4 5; do
  echo "Initializing tenant${i}_db..."
  pgbench -i -U postgres "tenant${i}_db"
done
