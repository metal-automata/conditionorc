/*
Copyright © 2023-2024 Metal Toolbox Authors <EMAIL ADDRESS>
Copyright 2024 Metal Automata Authors https://github.com/metal-automata

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package main

import "github.com/metal-automata/conditionorc/cmd"

// @BasePath /api/v1
// @title Condition orchestrator API
// @description Conditions API expose CRUD actions to condition objects on servers

func main() {
	cmd.Execute()
}
