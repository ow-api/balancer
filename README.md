Ow-API Balancer
===============
[![Build Status](https://github.drone.meow.tf/api/badges/ow-api/balancer/status.svg)](https://github.drone.meow.tf/ow-api/balancer)

A simple load balancer for Ow-API. This docker image embeds the [website](https://github.com/ow-api/website) and 
[docs](https://github.com/ow-api/docs), as well as routes requests to the specific 
[backends](https://github.com/ow-api/ow-api)

Website/Documentation
---------------------

The "data" directory is populated in the Dockerfile from the [website](https://github.com/ow-api/website) and 
[docs](https://github.com/ow-api/docs) repositories, which are then embedded into the Go binary.

Caching
-------

This load balancer uses caching to serve data from an internal cache. With Ow-API's setup, this means that the edge 
server can serve cached data (like haproxy + varnish) without it ever  going to a (potentially different) server. 

The goal of this is to reduce requests to Overwatch's website to avoid bans.