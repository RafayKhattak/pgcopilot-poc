#!/bin/bash
for i in 1 2 3 4 5; do
  pgbench -c 20 -j 2 -T 60 -U postgres "tenant${i}_db" &
done
wait
